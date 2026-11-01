package syncer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/domain"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
)

type candidateCall struct {
	targetURL  string
	sourceURL  string
	branchName string
	candidate  string
	targetSHA  string
	sourceSHA  string
}

type fakeCandidatePusher struct {
	calls []candidateCall
	errAt int
}

func (p *fakeCandidatePusher) PushCandidate(_ context.Context, targetURL string, sourceURL string, branchName string, candidateBranch string, targetSHA string, sourceSHA string) (gitinspect.Candidate, error) {
	p.calls = append(p.calls, candidateCall{
		targetURL:  targetURL,
		sourceURL:  sourceURL,
		branchName: branchName,
		candidate:  candidateBranch,
		targetSHA:  targetSHA,
		sourceSHA:  sourceSHA,
	})
	if p.errAt == len(p.calls) {
		return gitinspect.Candidate{}, errors.New("push refused")
	}
	return gitinspect.Candidate{
		BranchName: candidateBranch,
		CommitSHA:  strings.Repeat("c", 40),
	}, nil
}

type prCall struct {
	owner      string
	repository string
	head       string
	base       string
	title      string
	body       string
}

type fakePRManager struct {
	calls []prCall
	err   error
}

type fakeMergeChecker struct {
	allowed   bool
	denyOwner string
	err       error
	calls     []string
}

func (c *fakeMergeChecker) MergeCommitAllowed(_ context.Context, owner string, repository string) (bool, error) {
	c.calls = append(c.calls, owner+"/"+repository)
	return c.allowed && owner != c.denyOwner, c.err
}

func (m *fakePRManager) EnsurePullRequest(_ context.Context, owner string, repository string, headBranch string, baseBranch string, title string, body string) (github.PullRequestResult, error) {
	m.calls = append(m.calls, prCall{
		owner:      owner,
		repository: repository,
		head:       headBranch,
		base:       baseBranch,
		title:      title,
		body:       body,
	})
	if m.err != nil {
		return github.PullRequestResult{}, m.err
	}
	return github.PullRequestResult{Number: len(m.calls), URL: "https://github.com/" + owner + "/" + repository + "/pull/1", Created: true}, nil
}

func TestApplyCreatesNewCandidatesAndPRsForPlannedTargets(t *testing.T) {
	t.Parallel()

	pusher := &fakeCandidatePusher{}
	prManager := &fakePRManager{}
	mergeChecker := &fakeMergeChecker{allowed: true}
	executor := Executor{Git: pusher, PRs: prManager, MergeChecker: mergeChecker}
	results, err := executor.Apply(context.Background(), testRepository(), testBranch())
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(results) != 2 || len(pusher.calls) != 2 || len(prManager.calls) != 2 || len(mergeChecker.calls) != 2 {
		t.Fatalf("results=%d pushes=%d PRs=%d merge checks=%d, want 2 each", len(results), len(pusher.calls), len(prManager.calls), len(mergeChecker.calls))
	}
	if got, want := pusher.calls[0].targetURL, "https://github.com/Acme-Org/api-gateway.git"; got != want {
		t.Fatalf("first target URL = %q, want %q", got, want)
	}
	if got, want := pusher.calls[0].sourceURL, "https://github.com/Globex-Org/api-gateway.git"; got != want {
		t.Fatalf("first source URL = %q, want %q", got, want)
	}
	if got, want := pusher.calls[1].targetURL, "https://github.com/Globex-Org/api-gateway.git"; got != want {
		t.Fatalf("second target URL = %q, want %q", got, want)
	}
	if !strings.Contains(pusher.calls[0].candidate, "globex-to-acme/develop") || !strings.Contains(pusher.calls[1].candidate, "acme-to-globex/develop") {
		t.Fatalf("candidate branches = %q and %q, want explicit directions", pusher.calls[0].candidate, pusher.calls[1].candidate)
	}
	if !strings.Contains(prManager.calls[0].body, "Do not squash or rebase") || !strings.Contains(prManager.calls[0].body, "merge-commit method") {
		t.Fatalf("PR body does not warn against history rewriting: %q", prManager.calls[0].body)
	}
	if !reflect.DeepEqual([]string{results[0].TargetSide, results[1].TargetSide}, []string{"acme", "globex"}) {
		t.Fatalf("result targets = %#v", results)
	}
}

func TestApplyRejectsDevTestBeforeAnyRemoteWriter(t *testing.T) {
	t.Parallel()

	pusher := &fakeCandidatePusher{}
	prManager := &fakePRManager{}
	mergeChecker := &fakeMergeChecker{allowed: true}
	branch := testBranch()
	branch.Name = "dev-test"
	branch.Policy = "preserve_only"

	_, err := (Executor{Git: pusher, PRs: prManager, MergeChecker: mergeChecker}).Apply(context.Background(), testRepository(), branch)
	if err == nil || !strings.Contains(err.Error(), "not eligible") {
		t.Fatalf("Apply() error = %v, want preserve-only rejection", err)
	}
	if len(pusher.calls) != 0 || len(prManager.calls) != 0 || len(mergeChecker.calls) != 0 {
		t.Fatalf("dependencies were called: pushes=%d PRs=%d merge checks=%d", len(pusher.calls), len(prManager.calls), len(mergeChecker.calls))
	}
}

func TestApplyReturnsPartialProgressWithoutRollback(t *testing.T) {
	t.Parallel()

	pusher := &fakeCandidatePusher{errAt: 2}
	prManager := &fakePRManager{}
	mergeChecker := &fakeMergeChecker{allowed: true}
	results, err := (Executor{Git: pusher, PRs: prManager, MergeChecker: mergeChecker}).Apply(context.Background(), testRepository(), testBranch())
	if err == nil || !strings.Contains(err.Error(), "push refused") {
		t.Fatalf("Apply() error = %v, want second-target push error", err)
	}
	if len(results) != 1 || len(prManager.calls) != 1 {
		t.Fatalf("partial results=%d PRs=%d, want first target recorded only", len(results), len(prManager.calls))
	}
}

func TestApplyStopsBeforePushWhenMergeCommitsAreDisabled(t *testing.T) {
	t.Parallel()

	pusher := &fakeCandidatePusher{}
	prManager := &fakePRManager{}
	mergeChecker := &fakeMergeChecker{allowed: false}
	_, err := (Executor{Git: pusher, PRs: prManager, MergeChecker: mergeChecker}).Apply(context.Background(), testRepository(), testBranch())
	if err == nil || !strings.Contains(err.Error(), "does not allow merge commits") {
		t.Fatalf("Apply() error = %v, want merge-method safety stop", err)
	}
	if len(pusher.calls) != 0 || len(prManager.calls) != 0 {
		t.Fatalf("writers were called despite disabled merge commits: pushes=%d PRs=%d", len(pusher.calls), len(prManager.calls))
	}
}

func TestApplyPreflightsEveryTargetBeforeAnyPush(t *testing.T) {
	t.Parallel()

	pusher := &fakeCandidatePusher{}
	prManager := &fakePRManager{}
	mergeChecker := &fakeMergeChecker{allowed: true, denyOwner: "Globex-Org"}
	_, err := (Executor{Git: pusher, PRs: prManager, MergeChecker: mergeChecker}).Apply(context.Background(), testRepository(), testBranch())
	if err == nil || !strings.Contains(err.Error(), "does not allow merge commits") {
		t.Fatalf("Apply() error = %v, want second-target policy rejection", err)
	}
	if len(mergeChecker.calls) != 2 {
		t.Fatalf("merge checks = %#v, want both targets checked", mergeChecker.calls)
	}
	if len(pusher.calls) != 0 || len(prManager.calls) != 0 {
		t.Fatalf("remote writers ran before full preflight: pushes=%d PRs=%d", len(pusher.calls), len(prManager.calls))
	}
}

func TestApplyReportsCandidateWhenPRCreationFailsWithoutRollback(t *testing.T) {
	t.Parallel()

	pusher := &fakeCandidatePusher{}
	prManager := &fakePRManager{err: errors.New("PR API unavailable")}
	mergeChecker := &fakeMergeChecker{allowed: true}
	results, err := (Executor{Git: pusher, PRs: prManager, MergeChecker: mergeChecker}).Apply(context.Background(), testRepository(), testBranch())
	if err == nil || !strings.Contains(err.Error(), "PR API unavailable") {
		t.Fatalf("Apply() error = %v, want PR failure", err)
	}
	if len(results) != 1 || results[0].Candidate == "" || results[0].PullRequest != "" || !results[0].BranchCreated {
		t.Fatalf("partial result = %#v, want created candidate without PR", results)
	}
	if len(pusher.calls) != 1 {
		t.Fatalf("push attempts = %d, want one; execution should stop after PR error", len(pusher.calls))
	}
}

func testRepository() domain.RepositoryStatus {
	return domain.RepositoryStatus{
		Service: "api-gateway",
		Left: domain.RepositorySide{
			Label:      "acme",
			Owner:      "Acme-Org",
			Repository: "api-gateway",
		},
		Right: domain.RepositorySide{
			Label:      "globex",
			Owner:      "Globex-Org",
			Repository: "api-gateway",
		},
	}
}

func testBranch() domain.BranchStatus {
	return domain.BranchStatus{
		Name:       "develop",
		Policy:     "sync_candidate",
		LeftSHA:    strings.Repeat("a", 40),
		RightSHA:   strings.Repeat("b", 40),
		Assessment: "clean_merge",
		PRTargets:  []string{"acme", "globex"},
	}
}
