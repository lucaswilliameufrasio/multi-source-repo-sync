package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/domain"
)

func TestCompactReportKeepsCandidatesAndHidesInventory(t *testing.T) {
	t.Parallel()
	report := domain.Report{Mode: "plan", Repositories: []domain.RepositoryStatus{{
		Service: "cache-service",
		Branches: []domain.BranchStatus{
			{Name: "main", Policy: "sync_candidate", Assessment: "merge_conflict"},
			{Name: "develop", Policy: "sync_candidate", Assessment: "clean_merge", PRTargets: []string{"globex"}},
			{Name: "dev-test", Policy: "preserve_only", Assessment: "preserved"},
			{Name: "feature/very-long-name", Policy: "inventory_only", LeftSHA: strings.Repeat("a", 40)},
		},
		Errors: []string{"diagnostic\nmessage"},
	}}}
	var output bytes.Buffer
	if err := writeText(&output, report); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"cache-service", "CONFLITO", "COMBINÁVEL", "PR para globex", "2 branches: 0 iguais", "1 combináveis", "1 bloqueadas", "ERRO: diagnostic message", "--verbose"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
	for _, hidden := range []string{"dev-test", "feature/very-long-name", strings.Repeat("a", 12), "sync_candidate", "merge_conflict"} {
		if strings.Contains(text, hidden) {
			t.Errorf("compact report leaks %q", hidden)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if len([]rune(line)) > 90 {
			t.Errorf("line too wide: %q", line)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestCompactReportPropagatesWriteError(t *testing.T) {
	t.Parallel()
	if err := writeText(failingWriter{}, domain.Report{}); err == nil {
		t.Fatal("expected output failure")
	}
}

func TestBranchSummary(t *testing.T) {
	t.Parallel()
	for _, assessment := range []string{"in_sync", "different_heads", "clean_merge", "merge_conflict", "unrelated_histories", "missing_on_both", "missing_on_acme", "missing_on_globex", "repository_unavailable", "history_unavailable", "future_status"} {
		repository := domain.RepositoryStatus{Left: domain.RepositorySide{Label: "acme"}, Right: domain.RepositorySide{Label: "globex"}}
		if got := branchSummary(repository, domain.BranchStatus{Assessment: assessment}); got == "" {
			t.Errorf("empty summary for %s", assessment)
		}
	}
}

func TestDisplayModesPreserveFullInventory(t *testing.T) {
	t.Parallel()
	report := domain.Report{Mode: "status", Repositories: []domain.RepositoryStatus{{
		Service:  "service",
		Branches: []domain.BranchStatus{{Name: "dev-test", Policy: "preserve_only", Assessment: "preserved", LeftSHA: strings.Repeat("a", 40)}},
	}}}
	var verbose bytes.Buffer
	if err := writeReportText(&verbose, report, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(verbose.String(), "dev-test") || !strings.Contains(verbose.String(), strings.Repeat("a", 12)) {
		t.Fatalf("verbose inventory lost: %s", &verbose)
	}
	var structured bytes.Buffer
	if err := writeJSON(&structured, report); err != nil {
		t.Fatal(err)
	}
	var decoded domain.Report
	if err := json.Unmarshal(structured.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if got := decoded.Repositories[0].Branches[0]; got.Name != "dev-test" || got.Policy != "preserve_only" || got.LeftSHA != strings.Repeat("a", 40) {
		t.Fatalf("JSON inventory changed: %+v", got)
	}
}

type fakeMergeCommitChecker struct {
	values map[string]bool
	calls  []string
}

func (c *fakeMergeCommitChecker) MergeCommitAllowed(_ context.Context, owner string, repository string) (bool, error) {
	key := owner + "/" + repository
	c.calls = append(c.calls, key)
	return c.values[key], nil
}

func TestRunRejectsUnknownCommandWithoutExternalCalls(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), []string{"destroy"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("Run() exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q, want unknown command", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunRejectsDevTestBeforeExternalCalls(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), []string{
		"sync",
		"--config",
		"../../config/repos.example.json",
		"--service",
		"api-gateway",
		"--branch",
		"dev-test",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("Run() exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "not configured as a sync candidate") {
		t.Fatalf("stderr = %q, want preserve-only branch rejection", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestConfirmApplyRequiresExactTypedPhrase(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "exact phrase", input: "sync api-gateway develop acme/globex\n", want: true},
		{name: "wrong phrase", input: "apply\n", want: false},
		{name: "no confirmation", input: "", want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var prompt bytes.Buffer
			got, err := confirmApply(strings.NewReader(testCase.input), &prompt, "api-gateway", "develop", "acme/globex")
			if err != nil {
				t.Fatalf("confirmApply() error = %v", err)
			}
			if got != testCase.want {
				t.Fatalf("confirmApply() = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestCheckMergeCommitSupportChecksAllTargetsBeforeApply(t *testing.T) {
	t.Parallel()

	repository := domain.RepositoryStatus{
		Service: "api-gateway",
		Left:    domain.RepositorySide{Label: "acme", Owner: "Acme-Org", Repository: "api-gateway"},
		Right:   domain.RepositorySide{Label: "globex", Owner: "Globex-Org", Repository: "api-gateway"},
	}
	branch := domain.BranchStatus{Name: "develop", PRTargets: []string{"acme", "globex"}}
	checker := &fakeMergeCommitChecker{values: map[string]bool{
		"Acme-Org/api-gateway":   true,
		"Globex-Org/api-gateway": false,
	}}
	var output bytes.Buffer
	err := checkMergeCommitSupport(context.Background(), repository, branch, checker, &output)
	if err == nil || !strings.Contains(err.Error(), "does not allow merge commits") {
		t.Fatalf("checkMergeCommitSupport() error = %v, want blocked target", err)
	}
	if len(checker.calls) != 2 {
		t.Fatalf("checked targets = %#v, want both targets checked", checker.calls)
	}
	if !strings.Contains(output.String(), "allow_merge_commit=true") || !strings.Contains(output.String(), "allow_merge_commit=false") {
		t.Fatalf("merge-method report = %q, want both target settings", output.String())
	}
}

func TestWriteTextSanitizesPullRequestTitle(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	report := domain.Report{
		Mode: "status",
		Repositories: []domain.RepositoryStatus{{
			Service: "api-gateway",
			Left: domain.RepositorySide{
				Label:    "acme",
				Branches: []domain.BranchRef{{Name: "develop", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
				OpenPullRequests: []domain.PullRequest{{
					Side:       "acme",
					Number:     31,
					Title:      "first\n\x1b[31msecond",
					HeadBranch: "bugfix/security",
					BaseBranch: "develop",
					Draft:      true,
				}},
			},
			Right: domain.RepositorySide{Label: "globex"},
		}},
	}
	if err := writeVerboseText(&output, report); err != nil {
		t.Fatalf("writeText() error = %v", err)
	}
	if strings.Contains(output.String(), "\x1b") {
		t.Fatal("text output contains a terminal escape character")
	}
	if !strings.Contains(output.String(), "first second [draft]") {
		t.Fatalf("text output did not sanitize pull request title: %q", output.String())
	}
}

func TestBranchSummaryNamesTheOrganizationThatHasTheBranch(t *testing.T) {
	t.Parallel()
	repository := domain.RepositoryStatus{Left: domain.RepositorySide{Label: "initech"}, Right: domain.RepositorySide{Label: "globex"}}
	if got := branchSummary(repository, domain.BranchStatus{Assessment: "missing_on_initech"}); !strings.HasPrefix(got, "SÓ GLOBEX") {
		t.Errorf("summary = %q, want it to say the branch exists only in globex", got)
	}
	if got := branchSummary(repository, domain.BranchStatus{Assessment: "missing_on_globex"}); !strings.HasPrefix(got, "SÓ INITECH") {
		t.Errorf("summary = %q, want it to say the branch exists only in initech", got)
	}
}

func manifestWithThreeOrganizations() config.Config {
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

func TestChoosePairNeedsAChoiceOnlyWhenTheServiceHasSeveral(t *testing.T) {
	t.Parallel()
	cfg := manifestWithThreeOrganizations()

	var stderr bytes.Buffer
	if _, ok := choosePair(cfg, "api-gateway", "", &stderr); ok || !strings.Contains(stderr.String(), "initech/acme, initech/globex, acme/globex") {
		t.Fatalf("api-gateway has three pairs and no --pair: ok=%t stderr=%q", ok, stderr.String())
	}
	stderr.Reset()
	pair, ok := choosePair(cfg, "ledger-service", "", &stderr)
	if !ok || pair.Name() != "acme/globex" {
		t.Fatalf("ledger-service has one pair: %v %t %q", pair.Name(), ok, stderr.String())
	}
	stderr.Reset()
	pair, ok = choosePair(cfg, "api-gateway", "globex/initech", &stderr)
	if !ok || pair.Name() != "initech/globex" {
		t.Fatalf("a pair is found in either order and named as in the manifest: %v %t", pair.Name(), ok)
	}
	stderr.Reset()
	if _, ok := choosePair(cfg, "ledger-service", "initech/globex", &stderr); ok || !strings.Contains(stderr.String(), "no copy in both") {
		t.Fatalf("initech has no ledger-service: ok=%t stderr=%q", ok, stderr.String())
	}
	stderr.Reset()
	if _, ok := choosePair(cfg, "api-gateway", "nobody/globex", &stderr); ok || !strings.Contains(stderr.String(), "not configured") {
		t.Fatalf("unknown pair: ok=%t stderr=%q", ok, stderr.String())
	}
}

func TestScopeToPairListsOnlyThatPair(t *testing.T) {
	t.Parallel()
	cfg := manifestWithThreeOrganizations()
	pair, _ := cfg.Pair("initech/globex")
	scoped := scopeToPair(cfg, pair)
	if got := pairList(scoped); got != "initech/globex" {
		t.Fatalf("scoped pairs = %q, want only initech/globex", got)
	}
	if got := pairList(cfg); !strings.Contains(got, "acme/globex") {
		t.Fatalf("scoping must not change the original: %q", got)
	}
}

func TestAResolutionTargetMustBeOneOfThePair(t *testing.T) {
	t.Parallel()
	cfg := manifestWithThreeOrganizations()
	pair, _ := cfg.Pair("initech/acme")
	if source, err := otherLabel(pair, "acme"); err != nil || source != "initech" {
		t.Fatalf("otherLabel(acme) = %q, %v", source, err)
	}
	if _, err := otherLabel(pair, "globex"); err == nil || !strings.Contains(err.Error(), "never chooses") {
		t.Fatalf("globex is not in the pair: %v", err)
	}
	if err := validateResolutionScope(cfg, "ledger-service", "main", "acme", "initech"); err == nil {
		t.Fatal("initech has no ledger-service, so a resolution with it must be refused")
	}
	if err := validateResolutionScope(cfg, "api-gateway", "main", "acme", "initech"); err != nil {
		t.Fatalf("valid resolution refused: %v", err)
	}
}
