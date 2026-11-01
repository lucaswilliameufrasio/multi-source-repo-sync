package workflow

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
)

type fakeReader struct {
	mu    sync.Mutex
	heads map[string]map[string]string // owner/repo -> branch -> sha
	reads int
}

func (r *fakeReader) ReadRepository(_ context.Context, owner string, repository string, _ string) (github.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	branches, ok := r.heads[owner+"/"+repository]
	if !ok {
		return github.Snapshot{}, context.DeadlineExceeded
	}
	copied := make(map[string]string, len(branches))
	for name, sha := range branches {
		copied[name] = sha
	}
	return github.Snapshot{Branches: copied}, nil
}

type fakeInspector struct {
	comparison gitinspect.Comparison
	calls      int
}

func (i *fakeInspector) Compare(_ context.Context, _ string, _ string, heads []gitinspect.BranchHeads) (map[string]gitinspect.Comparison, error) {
	i.calls++
	out := map[string]gitinspect.Comparison{}
	for _, head := range heads {
		out[head.Name] = i.comparison
	}
	return out, nil
}

type fakeWrites struct {
	mu       sync.Mutex
	pushes   []string
	prs      []string
	allowed  map[string]bool
	mergeAsk int
}

func (w *fakeWrites) PushCandidate(_ context.Context, targetURL string, _ string, _ string, candidate string, _ string, _ string) (gitinspect.Candidate, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pushes = append(w.pushes, targetURL+" "+candidate)
	return gitinspect.Candidate{BranchName: candidate, CommitSHA: strings.Repeat("c", 40)}, nil
}

func (w *fakeWrites) EnsurePullRequest(_ context.Context, owner string, repository string, head string, base string, _ string, _ string) (github.PullRequestResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prs = append(w.prs, owner+"/"+repository+" "+head+" -> "+base)
	return github.PullRequestResult{URL: "https://github.com/" + owner + "/" + repository + "/pull/1", Created: true}, nil
}

func (w *fakeWrites) MergeCommitAllowed(_ context.Context, owner string, _ string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.mergeAsk++
	allowed, known := w.allowed[owner]
	return !known || allowed, nil
}

func manifest() config.Config {
	return config.Config{
		Version: 1,
		Organizations: config.Organizations{
			{Label: "initech", Owner: "Initech-Org"},
			{Label: "acme", Owner: "Acme-Org"},
			{Label: "globex", Owner: "Globex-Org"},
		},
		Services:             []string{"api-gateway", "ledger-service"},
		OrganizationServices: map[string][]string{"initech": {"api-gateway"}},
		SyncBranches:         []string{"main"},
		PreserveOnlyBranches: []string{"dev-test"},
	}
}

func heads(euf string, tix string) map[string]map[string]string {
	sha := func(c string) string { return strings.Repeat(c, 40) }
	return map[string]map[string]string{
		"Initech-Org/api-gateway": {"main": sha(euf)},
		"Acme-Org/api-gateway":    {"main": sha(tix)},
		"Globex-Org/api-gateway":  {"main": sha(tix)},
	}
}

func newWorkflow(reader *fakeReader, comparison gitinspect.Comparison) (Workflow, *fakeWrites, *fakeInspector) {
	writes := &fakeWrites{}
	inspector := &fakeInspector{comparison: comparison}
	return Workflow{Reader: reader, Inspector: inspector, Git: writes, PRs: writes, Merge: writes}, writes, inspector
}

func code(t *testing.T, err error) string {
	t.Helper()
	problem, ok := IsProblem(err)
	if !ok {
		t.Fatalf("error = %v, want a Problem", err)
	}
	return problem.Code
}

func TestProposeNamesTheWritesAndTheExactConfirmation(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{heads: heads("a", "b")}
	wf, writes, _ := newWorkflow(reader, gitinspect.Comparison{Relation: gitinspect.RelationLeftBehind})

	proposal, err := wf.Propose(context.Background(), manifest(), "api-gateway", "main", "initech/acme")
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}
	if proposal.Blocked != "" || len(proposal.Writes) != 1 {
		t.Fatalf("proposal = %+v, want one write and no block", proposal)
	}
	write := proposal.Writes[0]
	if write.TargetLabel != "initech" || write.TargetRepository != "Initech-Org/api-gateway" || write.SourceRepository != "Acme-Org/api-gateway" {
		t.Fatalf("write = %+v, want Acme's changes into Initech-Org", write)
	}
	if !strings.HasPrefix(write.CandidateBranch, "sync/multi-source-repo-sync/acme-to-initech/main/") {
		t.Fatalf("candidate = %q", write.CandidateBranch)
	}
	if proposal.Phrase != "sync api-gateway main initech/acme" {
		t.Fatalf("phrase = %q", proposal.Phrase)
	}
	if len(writes.pushes) != 0 || len(writes.prs) != 0 {
		t.Fatalf("Propose wrote: %v %v", writes.pushes, writes.prs)
	}
}

func TestAConflictOrAMissingMergeCommitSettingBlocksAndWritesNothing(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{heads: heads("a", "b")}
	wf, writes, _ := newWorkflow(reader, gitinspect.Comparison{Relation: gitinspect.RelationDiverged, MergeConflict: true})
	proposal, err := wf.Propose(context.Background(), manifest(), "api-gateway", "main", "initech/acme")
	if err != nil || proposal.Blocked == "" || len(proposal.Writes) != 0 {
		t.Fatalf("conflict: %+v, %v", proposal, err)
	}
	if writes.mergeAsk != 0 {
		t.Fatal("a conflict must not even ask about merge commits")
	}

	wf, writes, _ = newWorkflow(reader, gitinspect.Comparison{Relation: gitinspect.RelationLeftBehind})
	writes.allowed = map[string]bool{"Initech-Org": false}
	proposal, err = wf.Propose(context.Background(), manifest(), "api-gateway", "main", "initech/acme")
	if err != nil || !strings.Contains(proposal.Blocked, "does not allow merge commits") {
		t.Fatalf("merge commits off: %+v, %v", proposal, err)
	}
}

func TestOnlyAConfiguredBranchAndPairCanBeSynchronized(t *testing.T) {
	t.Parallel()
	wf, _, _ := newWorkflow(&fakeReader{heads: heads("a", "b")}, gitinspect.Comparison{})
	cases := []struct {
		name, service, branch, pair, want string
	}{
		{"dev-test", "api-gateway", "dev-test", "initech/acme", "branch_not_candidate"},
		{"unlisted branch", "api-gateway", "feature/x", "initech/acme", "branch_not_candidate"},
		{"unknown service", "nope", "main", "initech/acme", "unknown_service"},
		{"several pairs and none chosen", "api-gateway", "main", "", "pair_required"},
		{"pair that does not exist", "api-gateway", "main", "x/y", "unknown_pair"},
		{"pair without the service", "ledger-service", "main", "initech/globex", "service_not_in_pair"},
	}
	for _, testCase := range cases {
		_, err := wf.Propose(context.Background(), manifest(), testCase.service, testCase.branch, testCase.pair)
		if got := code(t, err); got != testCase.want {
			t.Errorf("%s: code = %q, want %q", testCase.name, got, testCase.want)
		}
	}
	// A service in a single pair needs no --pair.
	if pair, err := ChoosePair(manifest(), "ledger-service", ""); err != nil || pair.Name() != "acme/globex" {
		t.Fatalf("ChoosePair(ledger-service) = %v, %v", pair.Name(), err)
	}
}

func TestApplyNeedsTheExactPhraseAndWritesNothingWithoutIt(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{heads: heads("a", "b")}
	wf, writes, _ := newWorkflow(reader, gitinspect.Comparison{Relation: gitinspect.RelationLeftBehind})
	request := ApplyRequest{
		Service: "api-gateway", Branch: "main", Pair: "initech/acme",
		LeftSHA: strings.Repeat("a", 40), RightSHA: strings.Repeat("b", 40),
	}
	for _, wrong := range []string{"", "sync api-gateway main", "sync api-gateway main acme/initech", "SYNC api-gateway main initech/acme", " sync api-gateway main initech/acme"} {
		request.Confirmation = wrong
		_, err := wf.Apply(context.Background(), manifest(), request)
		if got := code(t, err); got != "confirmation_mismatch" {
			t.Errorf("phrase %q: code = %q", wrong, got)
		}
	}
	if len(writes.pushes)+len(writes.prs) != 0 || reader.reads != 0 {
		t.Fatalf("a wrong phrase must not even read GitHub: %v %v reads=%d", writes.pushes, writes.prs, reader.reads)
	}
}

func TestApplyPlansAgainAndRefusesWhenTheHeadsMovedSinceTheReview(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{heads: heads("a", "c")} // acme moved from b to c after the review
	wf, writes, _ := newWorkflow(reader, gitinspect.Comparison{Relation: gitinspect.RelationLeftBehind})
	_, err := wf.Apply(context.Background(), manifest(), ApplyRequest{
		Service: "api-gateway", Branch: "main", Pair: "initech/acme",
		Confirmation: "sync api-gateway main initech/acme",
		LeftSHA:      strings.Repeat("a", 40), RightSHA: strings.Repeat("b", 40),
	})
	if got := code(t, err); got != "plan_changed" {
		t.Fatalf("code = %q, want plan_changed", got)
	}
	if len(writes.pushes)+len(writes.prs) != 0 {
		t.Fatalf("a moved head must write nothing: %v %v", writes.pushes, writes.prs)
	}
}

func TestApplyPushesTheCandidateAndOpensThePullRequest(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{heads: heads("a", "b")}
	wf, writes, inspector := newWorkflow(reader, gitinspect.Comparison{Relation: gitinspect.RelationLeftBehind})
	results, err := wf.Apply(context.Background(), manifest(), ApplyRequest{
		Service: "api-gateway", Branch: "main", Pair: "initech/acme",
		Confirmation: "sync api-gateway main initech/acme",
		LeftSHA:      strings.Repeat("a", 40), RightSHA: strings.Repeat("b", 40),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(results) != 1 || results[0].TargetSide != "initech" || !results[0].PRCreated || results[0].PullRequest == "" {
		t.Fatalf("results = %+v", results)
	}
	if len(writes.pushes) != 1 || !strings.Contains(writes.pushes[0], "Initech-Org/api-gateway") {
		t.Fatalf("pushes = %v, want one into Initech-Org", writes.pushes)
	}
	if len(writes.prs) != 1 || !strings.HasSuffix(writes.prs[0], "-> main") {
		t.Fatalf("pull requests = %v", writes.prs)
	}
	if inspector.calls != 1 {
		t.Fatalf("history inspected %d times, want once (planned again after the confirmation)", inspector.calls)
	}
}

func TestApplyRefusesAConflict(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{heads: heads("a", "b")}
	wf, writes, _ := newWorkflow(reader, gitinspect.Comparison{Relation: gitinspect.RelationDiverged, MergeConflict: true})
	_, err := wf.Apply(context.Background(), manifest(), ApplyRequest{
		Service: "api-gateway", Branch: "main", Pair: "initech/acme",
		Confirmation: "sync api-gateway main initech/acme",
		LeftSHA:      strings.Repeat("a", 40), RightSHA: strings.Repeat("b", 40),
	})
	if got := code(t, err); got != "not_clean" {
		t.Fatalf("code = %q, want not_clean", got)
	}
	if len(writes.pushes)+len(writes.prs) != 0 {
		t.Fatal("a conflict must write nothing")
	}
}
