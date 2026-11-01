package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
)

type fakeBranches struct {
	mu     sync.Mutex
	pushed []string
	fail   map[string]error // by target URL fragment
}

func (b *fakeBranches) PushBranch(_ context.Context, targetURL string, sourceURL string, branch string, sha string, base string) (gitinspect.BranchPush, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for fragment, err := range b.fail {
		if strings.Contains(targetURL, fragment) {
			return gitinspect.BranchPush{}, err
		}
	}
	b.pushed = append(b.pushed, targetURL+" <- "+sourceURL+" "+branch+" "+sha[:7]+" base="+base)
	return gitinspect.BranchPush{BranchName: branch, SHA: sha, Ahead: 3}, nil
}

const feature = "feat/edge"

func propagationHeads(extra map[string]map[string]string) map[string]map[string]string {
	sha := func(c string) string { return strings.Repeat(c, 40) }
	heads := map[string]map[string]string{
		"Initech-Org/api-gateway": {"main": sha("a"), feature: sha("f")},
		"Acme-Org/api-gateway":    {"main": sha("b")},
		"Globex-Org/api-gateway":  {"main": sha("c")},
	}
	for repository, branches := range extra {
		for name, value := range branches {
			if value == "" {
				delete(heads[repository], name)
			} else {
				heads[repository][name] = value
			}
		}
	}
	return heads
}

func propagator(heads map[string]map[string]string) (Workflow, *fakeWrites, *fakeBranches) {
	writes := &fakeWrites{}
	branches := &fakeBranches{}
	return Workflow{Reader: &fakeReader{heads: heads}, Inspector: &fakeInspector{}, Git: writes, PRs: writes, Merge: writes, Branches: branches}, writes, branches
}

func states(proposal PropagateProposal) map[string]string {
	out := map[string]string{}
	for _, target := range proposal.Targets {
		out[target.Label] = target.State
	}
	return out
}

func TestProposePropagationFindsEveryOtherCopyAndItsState(t *testing.T) {
	t.Parallel()
	sha := func(c string) string { return strings.Repeat(c, 40) }
	wf, _, _ := propagator(propagationHeads(map[string]map[string]string{
		"Globex-Org/api-gateway": {feature: sha("f")}, // already has this exact commit
	}))
	proposal, err := wf.ProposePropagation(context.Background(), manifest(), "api-gateway", feature, "initech", nil, "")
	if err != nil {
		t.Fatalf("ProposePropagation() error = %v", err)
	}
	if proposal.Base != "main" || proposal.SourceSHA != sha("f") {
		t.Fatalf("proposal = %+v", proposal)
	}
	got := states(proposal)
	if got["acme"] != StateNew || got["globex"] != StateAlreadyThere || len(got) != 2 {
		t.Fatalf("states = %v, want acme new and globex already there", got)
	}
	if proposal.Phrase != "propagate api-gateway feat/edge from initech to acme,globex" {
		t.Fatalf("phrase = %q", proposal.Phrase)
	}
}

func TestPropagationStatesForOtherHistoryNoBaseAndMergeCommitsOff(t *testing.T) {
	t.Parallel()
	sha := func(c string) string { return strings.Repeat(c, 40) }
	wf, _, _ := propagator(propagationHeads(map[string]map[string]string{
		"Acme-Org/api-gateway":   {feature: sha("9")}, // a branch of that name with other commits
		"Globex-Org/api-gateway": {"main": ""},        // no base to propose into
	}))
	proposal, err := wf.ProposePropagation(context.Background(), manifest(), "api-gateway", feature, "initech", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := states(proposal); got["acme"] != StateDifferentHistory || got["globex"] != StateNoBase {
		t.Fatalf("states = %v", got)
	}

	var writes *fakeWrites
	wf, writes, _ = propagator(propagationHeads(nil))
	writes.allowed = map[string]bool{"Globex-Org": false}
	proposal, _ = wf.ProposePropagation(context.Background(), manifest(), "api-gateway", feature, "initech", nil, "")
	if got := states(proposal); got["globex"] != StateMergeCommitsOff || got["acme"] != StateNew {
		t.Fatalf("states = %v", got)
	}
}

func TestOnlyAnOrdinaryBranchOfARealSourceCanBePropagated(t *testing.T) {
	t.Parallel()
	wf, _, _ := propagator(propagationHeads(nil))
	cases := []struct{ name, service, branch, source, base, want string }{
		{"a sync branch", "api-gateway", "main", "initech", "", "branch_reserved"},
		{"dev-test", "api-gateway", "dev-test", "initech", "", "branch_reserved"},
		{"a candidate branch", "api-gateway", "sync/multi-source-repo-sync/x", "initech", "", "branch_invalid"},
		{"a bad name", "api-gateway", "a..b", "initech", "", "branch_invalid"},
		{"a name with a space", "api-gateway", "my branch", "initech", "", "branch_invalid"},
		{"unknown service", "nope", feature, "initech", "", "unknown_service"},
		{"unknown organization", "api-gateway", feature, "nobody", "", "unknown_organization"},
		{"a source without the service", "ledger-service", feature, "initech", "", "service_not_in_source"},
		{"a base that is not allowed", "api-gateway", feature, "initech", "release", "base_not_allowed"},
		{"a branch the source lacks", "api-gateway", "feat/missing", "initech", "", "branch_not_in_source"},
	}
	for _, testCase := range cases {
		_, err := wf.ProposePropagation(context.Background(), manifest(), testCase.service, testCase.branch, testCase.source, nil, testCase.base)
		if got := code(t, err); got != testCase.want {
			t.Errorf("%s: code = %q, want %q", testCase.name, got, testCase.want)
		}
	}
	if _, err := wf.ProposePropagation(context.Background(), manifest(), "api-gateway", feature, "initech", []string{"initech"}, ""); code(t, err) != "same_organization" {
		t.Error("the source cannot be its own target")
	}
}

func TestApplyPropagationNeedsThePhraseAndTheReviewedCommit(t *testing.T) {
	t.Parallel()
	sha := func(c string) string { return strings.Repeat(c, 40) }
	wf, writes, branches := propagator(propagationHeads(nil))
	request := PropagateRequest{Service: "api-gateway", Branch: feature, Source: "initech", SourceSHA: sha("f"),
		Confirmation: "propagate api-gateway feat/edge from initech to acme,globex"}

	wrong := request
	wrong.Confirmation = "propagate api-gateway feat/edge from initech to acme"
	if _, err := wf.ApplyPropagation(context.Background(), manifest(), wrong); code(t, err) != "confirmation_mismatch" {
		t.Error("a phrase naming fewer targets must be refused")
	}
	moved := request
	moved.SourceSHA = sha("e")
	if _, err := wf.ApplyPropagation(context.Background(), manifest(), moved); code(t, err) != "plan_changed" {
		t.Error("a source that moved since the review must be refused")
	}
	if len(branches.pushed) != 0 || len(writes.prs) != 0 {
		t.Fatalf("neither refusal may write: %v %v", branches.pushed, writes.prs)
	}
}

func TestApplyPropagationCreatesTheBranchAndThePullRequestInEachCopyAndReportsTheRest(t *testing.T) {
	t.Parallel()
	sha := func(c string) string { return strings.Repeat(c, 40) }
	wf, writes, branches := propagator(propagationHeads(map[string]map[string]string{
		"Globex-Org/api-gateway": {feature: sha("9")}, // other commits under that name: left alone
	}))
	results, err := wf.ApplyPropagation(context.Background(), manifest(), PropagateRequest{
		Service: "api-gateway", Branch: feature, Source: "initech", SourceSHA: sha("f"),
		Confirmation: "propagate api-gateway feat/edge from initech to acme,globex",
	})
	if err != nil {
		t.Fatalf("ApplyPropagation() error = %v", err)
	}
	if len(results) != 2 || results[0].Label != "acme" || results[0].Status != "created" || !results[0].PRCreated || results[0].PullRequest == "" {
		t.Fatalf("results = %+v", results)
	}
	if results[1].Label != "globex" || results[1].Status != "skipped" || !strings.Contains(results[1].Detail, "not touched") {
		t.Fatalf("results[1] = %+v, want globex left alone", results[1])
	}
	if len(branches.pushed) != 1 || !strings.Contains(branches.pushed[0], "Acme-Org/api-gateway") || !strings.HasSuffix(branches.pushed[0], "base=main") {
		t.Fatalf("pushed = %v, want only acme, into main", branches.pushed)
	}
	if len(writes.prs) != 1 || !strings.Contains(writes.prs[0], "feat/edge -> main") {
		t.Fatalf("pull requests = %v", writes.prs)
	}
}

func TestOneCopyFailingDoesNotStopTheOthers(t *testing.T) {
	t.Parallel()
	sha := func(c string) string { return strings.Repeat(c, 40) }
	wf, _, branches := propagator(propagationHeads(nil))
	branches.fail = map[string]error{"Acme-Org": errors.New("push refused")}
	results, err := wf.ApplyPropagation(context.Background(), manifest(), PropagateRequest{
		Service: "api-gateway", Branch: feature, Source: "initech", SourceSHA: sha("f"),
		Confirmation: "propagate api-gateway feat/edge from initech to acme,globex",
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != "failed" || !strings.Contains(results[0].Detail, "push refused") || results[1].Status != "created" {
		t.Fatalf("results = %+v, want acme failed and globex created", results)
	}
}
