package syncer

import (
	"context"
	"fmt"
	"strings"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/domain"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
)

type CandidatePusher interface {
	PushCandidate(ctx context.Context, targetURL string, sourceURL string, branchName string, candidateBranch string, targetSHA string, sourceSHA string) (gitinspect.Candidate, error)
}

type PullRequestManager interface {
	EnsurePullRequest(ctx context.Context, owner string, repository string, headBranch string, baseBranch string, title string, body string) (github.PullRequestResult, error)
}

type MergeCommitChecker interface {
	MergeCommitAllowed(ctx context.Context, owner string, repository string) (bool, error)
}

type Result struct {
	TargetSide    string `json:"target_side"`
	Candidate     string `json:"candidate_branch"`
	CommitSHA     string `json:"commit_sha"`
	BranchCreated bool   `json:"branch_created"`
	PullRequest   string `json:"pull_request_url,omitempty"`
	PRCreated     bool   `json:"pull_request_created"`
}

type Executor struct {
	Git          CandidatePusher
	PRs          PullRequestManager
	MergeChecker MergeCommitChecker
}

func (e Executor) Apply(ctx context.Context, repository domain.RepositoryStatus, branch domain.BranchStatus) ([]Result, error) {
	if e.Git == nil || e.PRs == nil || e.MergeChecker == nil {
		return nil, fmt.Errorf("git candidate pusher, pull request manager, and merge-commit checker are required")
	}
	if branch.Policy != "sync_candidate" || branch.Name == "dev-test" {
		return nil, fmt.Errorf("branch %q is not eligible for synchronization", branch.Name)
	}
	if branch.Assessment != "clean_merge" {
		return nil, fmt.Errorf("branch %q is not a clean merge candidate", branch.Name)
	}
	if len(branch.PRTargets) == 0 {
		return nil, fmt.Errorf("branch %q has no pull request targets", branch.Name)
	}

	type targetOperation struct {
		side      string
		target    domain.RepositorySide
		source    domain.RepositorySide
		targetSHA string
		sourceSHA string
		candidate string
	}
	operations := make([]targetOperation, 0, len(branch.PRTargets))
	for _, targetSide := range branch.PRTargets {
		target, source, targetSHA, sourceSHA, err := sidesForTarget(repository, branch, targetSide)
		if err != nil {
			return nil, err
		}
		mergeCommitAllowed, err := e.MergeChecker.MergeCommitAllowed(ctx, target.Owner, target.Repository)
		if err != nil {
			return nil, fmt.Errorf("check merge-commit support in %s/%s: %w", target.Owner, target.Repository, err)
		}
		if !mergeCommitAllowed {
			return nil, fmt.Errorf("%s/%s does not allow merge commits; stopping before any branch push to preserve commit history", target.Owner, target.Repository)
		}

		operations = append(operations, targetOperation{
			side:      targetSide,
			target:    target,
			source:    source,
			targetSHA: targetSHA,
			sourceSHA: sourceSHA,
			candidate: CandidateBranchName(targetSide, source.Label, branch.Name, targetSHA, sourceSHA),
		})
	}

	results := make([]Result, 0, len(operations))
	for _, operation := range operations {
		candidate, err := e.Git.PushCandidate(
			ctx,
			github.RepositoryURL(operation.target.Owner, operation.target.Repository),
			github.RepositoryURL(operation.source.Owner, operation.source.Repository),
			branch.Name,
			operation.candidate,
			operation.targetSHA,
			operation.sourceSHA,
		)
		if err != nil {
			if candidate.BranchName != "" {
				results = append(results, Result{
					TargetSide:    operation.side,
					Candidate:     candidate.BranchName,
					CommitSHA:     candidate.CommitSHA,
					BranchCreated: !candidate.AlreadyPushed,
				})
			}
			return results, fmt.Errorf("prepare %s PR candidate: %w", operation.side, err)
		}

		result := Result{
			TargetSide:    operation.side,
			Candidate:     candidate.BranchName,
			CommitSHA:     candidate.CommitSHA,
			BranchCreated: !candidate.AlreadyPushed,
		}
		title, body := pullRequestText(repository.Service, branch.Name, operation.target, operation.source, operation.sourceSHA, operation.targetSHA)
		pullRequest, err := e.PRs.EnsurePullRequest(
			ctx,
			operation.target.Owner,
			operation.target.Repository,
			candidate.BranchName,
			branch.Name,
			title,
			body,
		)
		if err != nil {
			results = append(results, result)
			return results, fmt.Errorf("candidate branch %q exists on %s but PR creation/check failed: %w", candidate.BranchName, operation.side, err)
		}
		result.PullRequest = pullRequest.URL
		result.PRCreated = pullRequest.Created
		results = append(results, result)
	}

	return results, nil
}

func sidesForTarget(repository domain.RepositoryStatus, branch domain.BranchStatus, targetLabel string) (domain.RepositorySide, domain.RepositorySide, string, string, error) {
	switch targetLabel {
	case repository.Left.Label:
		return repository.Left, repository.Right, branch.LeftSHA, branch.RightSHA, nil
	case repository.Right.Label:
		return repository.Right, repository.Left, branch.RightSHA, branch.LeftSHA, nil
	default:
		return domain.RepositorySide{}, domain.RepositorySide{}, "", "", fmt.Errorf("unknown pull request target %q", targetLabel)
	}
}

// CandidateBranchName names the branch a pull request is opened from. It carries both labels, so
// pairs that share an organization never produce the same name.
func CandidateBranchName(targetLabel string, sourceLabel string, branchName string, targetSHA string, sourceSHA string) string {
	return "sync/multi-source-repo-sync/" + sourceLabel + "-to-" + targetLabel + "/" + branchName + "/" + shortSHA(targetSHA) + "-" + shortSHA(sourceSHA)
}

func pullRequestText(service string, branch string, target domain.RepositorySide, source domain.RepositorySide, sourceSHA string, targetSHA string) (string, string) {
	title := fmt.Sprintf("Sync %s/%s %s from %s", target.Owner, service, branch, source.Owner)
	body := strings.Join([]string{
		"This synchronization proposal preserves the target and source commit histories.",
		"",
		"Target: " + target.Owner + "/" + service + " " + branch + " at " + targetSHA,
		"Source: " + source.Owner + "/" + service + " " + branch + " at " + sourceSHA,
		"",
		"If merging this PR, use the GitHub merge-commit method. Do not squash or rebase: the original commit identities must remain reachable.",
		"No automatic merge was requested.",
	}, "\n")
	return title, body
}

func shortSHA(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}
