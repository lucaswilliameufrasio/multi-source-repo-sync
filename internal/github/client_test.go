package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type runnerResult struct {
	output []byte
	err    error
}

type fakeRunner struct {
	results []runnerResult
	args    [][]string
}

func (r *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.args = append(r.args, append([]string{name}, args...))
	if len(r.results) == 0 {
		return nil, errors.New("unexpected command")
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result.output, result.err
}

func TestReadRepositoryDecodesPaginatedBranchesAndPullRequests(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{results: []runnerResult{
		{output: []byte(`[[{"name":"main","commit":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}],[{"name":"develop","commit":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}]]`)},
		{output: []byte(`[[{"number":12,"title":"sync","draft":true,"html_url":"https://github.com/Acme-Org/api-gateway/pull/12","head":{"ref":"feature/sync"},"base":{"ref":"develop"}}]]`)},
	}}
	client := NewClient(runner)

	snapshot, err := client.ReadRepository(context.Background(), "Acme-Org", "api-gateway", "acme")
	if err != nil {
		t.Fatalf("ReadRepository() error = %v", err)
	}
	if got, want := snapshot.Branches["develop"], "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"; got != want {
		t.Fatalf("develop SHA = %q, want %q", got, want)
	}
	if len(snapshot.Branches) != 2 {
		t.Fatalf("got %d branches, want 2", len(snapshot.Branches))
	}
	wantPullRequest := []struct {
		side  string
		num   int
		head  string
		base  string
		draft bool
	}{
		{side: "acme", num: 12, head: "feature/sync", base: "develop", draft: true},
	}
	if len(snapshot.OpenPullRequests) != len(wantPullRequest) {
		t.Fatalf("got %d pull requests, want %d", len(snapshot.OpenPullRequests), len(wantPullRequest))
	}
	gotPR := snapshot.OpenPullRequests[0]
	if gotPR.Side != wantPullRequest[0].side || gotPR.Number != wantPullRequest[0].num || gotPR.HeadBranch != wantPullRequest[0].head || gotPR.BaseBranch != wantPullRequest[0].base || gotPR.Draft != wantPullRequest[0].draft {
		t.Fatalf("decoded pull request = %+v", gotPR)
	}
	if got, want := len(runner.args), 2; got != want {
		t.Fatalf("runner call count = %d, want %d", got, want)
	}
	wantFirstArgs := []string{"gh", "api", "--paginate", "--slurp", "repos/Acme-Org/api-gateway/branches?per_page=100"}
	if !reflect.DeepEqual(runner.args[0], wantFirstArgs) {
		t.Fatalf("first args = %#v, want %#v", runner.args[0], wantFirstArgs)
	}
}

func TestReadRepositoryDoesNotExposeCommandStderr(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{results: []runnerResult{
		{err: errors.New("command failed with exit code 1")},
	}}
	client := NewClient(runner)

	_, err := client.ReadRepository(context.Background(), "Acme-Org", "api-gateway", "acme")
	if err == nil || !strings.Contains(err.Error(), "exit code 1") {
		t.Fatalf("ReadRepository() error = %v, want safe exit status", err)
	}
	if strings.Contains(err.Error(), "stderr") || strings.Contains(err.Error(), "token") {
		t.Fatalf("error contains potentially sensitive command output: %v", err)
	}
}

func TestRepositoryURL(t *testing.T) {
	t.Parallel()

	if got, want := RepositoryURL("Acme-Org", "api-gateway"), "https://github.com/Acme-Org/api-gateway.git"; got != want {
		t.Fatalf("RepositoryURL() = %q, want %q", got, want)
	}
}

func TestEnsurePullRequestReusesAnOpenPR(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{results: []runnerResult{
		{output: []byte(`[[{"number":42,"html_url":"https://github.com/Acme-Org/api-gateway/pull/42","head":{"ref":"sync/candidate"},"base":{"ref":"develop"}}]]`)},
	}}
	client := NewClient(runner)

	result, err := client.EnsurePullRequest(context.Background(), "Acme-Org", "api-gateway", "sync/candidate", "develop", "sync", "body")
	if err != nil {
		t.Fatalf("EnsurePullRequest() error = %v", err)
	}
	if result.Number != 42 || result.Created || result.URL != "https://github.com/Acme-Org/api-gateway/pull/42" {
		t.Fatalf("result = %+v, want existing PR #42", result)
	}
	if len(runner.args) != 1 {
		t.Fatalf("runner call count = %d, want only the read request", len(runner.args))
	}
	if !strings.Contains(runner.args[0][4], "head=Acme-Org%3Async%2Fcandidate") {
		t.Fatalf("existing PR query did not filter by owner and head: %#v", runner.args[0])
	}
}

func TestEnsurePullRequestCreatesWhenNoOpenPRExists(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{results: []runnerResult{
		{output: []byte(`[[]]`)},
		{output: []byte(`{"number":43,"html_url":"https://github.com/Globex-Org/ledger-service/pull/43"}`)},
	}}
	client := NewClient(runner)

	result, err := client.EnsurePullRequest(context.Background(), "Globex-Org", "ledger-service", "sync/candidate", "develop", "sync title", "sync body")
	if err != nil {
		t.Fatalf("EnsurePullRequest() error = %v", err)
	}
	if result.Number != 43 || !result.Created || result.URL != "https://github.com/Globex-Org/ledger-service/pull/43" {
		t.Fatalf("result = %+v, want created PR #43", result)
	}
	if len(runner.args) != 2 {
		t.Fatalf("runner call count = %d, want read and create calls", len(runner.args))
	}
	postArgs := strings.Join(runner.args[1], " ")
	if !strings.Contains(postArgs, "--method POST") || !strings.Contains(postArgs, "title=sync title") || !strings.Contains(postArgs, "body=sync body") {
		t.Fatalf("create request args = %q, missing expected fields", postArgs)
	}
}

func TestMergeCommitAllowedDecodesRepositorySetting(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{results: []runnerResult{{output: []byte("true\n")}}}
	client := NewClient(runner)
	allowed, err := client.MergeCommitAllowed(context.Background(), "Acme-Org", "api-gateway")
	if err != nil {
		t.Fatalf("MergeCommitAllowed() error = %v", err)
	}
	if !allowed {
		t.Fatal("MergeCommitAllowed() = false, want true")
	}
	wantArgs := []string{"gh", "api", "--jq", ".allow_merge_commit", "repos/Acme-Org/api-gateway"}
	if !reflect.DeepEqual(runner.args[0], wantArgs) {
		t.Fatalf("args = %#v, want %#v", runner.args[0], wantArgs)
	}
}

func TestMergeCommitAllowedRejectsMalformedResponse(t *testing.T) {
	t.Parallel()

	client := NewClient(&fakeRunner{results: []runnerResult{{output: []byte("unknown")}}})
	_, err := client.MergeCommitAllowed(context.Background(), "Globex-Org", "ledger-service")
	if err == nil || !strings.Contains(err.Error(), "decode merge-commit setting") {
		t.Fatalf("MergeCommitAllowed() error = %v, want decode error", err)
	}
}
