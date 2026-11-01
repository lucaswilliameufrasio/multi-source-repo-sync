// Package workflow is the safe way to synchronize one service, one branch and one pair of
// organizations: plan, check, ask for the exact confirmation, plan again, and only then write.
// The command line and the local web interface both go through it, so the rules cannot drift.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/app"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/domain"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/syncer"
)

// Problem is something the person can fix or must know; its text is safe to show.
type Problem struct {
	Code    string
	Message string
}

func (p *Problem) Error() string { return p.Message }

func problem(code string, format string, args ...any) *Problem {
	return &Problem{Code: code, Message: fmt.Sprintf(format, args...)}
}

// IsProblem reports whether err is a Problem (as opposed to something unexpected).
func IsProblem(err error) (*Problem, bool) {
	var p *Problem
	if errors.As(err, &p) {
		return p, true
	}
	return nil, false
}

type Workflow struct {
	Reader    github.RepositoryReader
	Inspector gitinspect.Inspector
	Git       syncer.CandidatePusher
	PRs       syncer.PullRequestManager
	Merge     syncer.MergeCommitChecker
	// Branches puts a whole branch into another repository (propagation).
	Branches BranchPusher
}

// Phrase is what the person types to allow the writes of one service, branch and pair.
func Phrase(service string, branch string, pair string) string {
	return "sync " + service + " " + branch + " " + pair
}

// ProposedWrite is one branch pushed and one pull request opened.
type ProposedWrite struct {
	TargetLabel        string `json:"target_label"`
	TargetRepository   string `json:"target_repository"`
	SourceRepository   string `json:"source_repository"`
	CandidateBranch    string `json:"candidate_branch"`
	BaseBranch         string `json:"base_branch"`
	SourceSHA          string `json:"source_sha"`
	MergeCommitAllowed bool   `json:"merge_commit_allowed"`
}

// Proposal is what a sync would do, with nothing done yet.
type Proposal struct {
	Service    string                  `json:"service"`
	Pair       string                  `json:"pair"`
	Branch     domain.BranchStatus     `json:"branch"`
	Repository domain.RepositoryStatus `json:"repository"`
	// Writes is empty unless the branch can be combined without conflicts.
	Writes []ProposedWrite `json:"writes"`
	// Blocked says why nothing can be written, when that is so.
	Blocked string `json:"blocked,omitempty"`
	// Phrase is the confirmation Apply asks for.
	Phrase string `json:"phrase"`
}

// ChoosePair picks the pair a sync works on; it must be given when the service is in several.
func ChoosePair(cfg config.Config, service string, pairName string) (config.Pair, error) {
	if !contains(cfg.Services, service) {
		return config.Pair{}, problem("unknown_service", "service %q is not in the configured allowlist", service)
	}
	available := cfg.PairsFor(service)
	if pairName != "" {
		pair, ok := cfg.Pair(pairName)
		if !ok {
			return config.Pair{}, problem("unknown_pair", "pair %q is not configured; the pairs are: %s", pairName, pairNames(cfg.Pairs()))
		}
		for _, candidate := range available {
			if candidate.Name() == pair.Name() {
				return pair, nil
			}
		}
		return config.Pair{}, problem("service_not_in_pair", "service %q has no copy in both organizations of %s", service, pair.Name())
	}
	if len(available) == 1 {
		return available[0], nil
	}
	return config.Pair{}, problem("pair_required", "service %q is in several pairs; choose one: %s", service, pairNames(available))
}

func pairNames(pairs []config.Pair) string {
	names := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		names = append(names, pair.Name())
	}
	return strings.Join(names, ", ")
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func checkBranch(cfg config.Config, branch string) error {
	if !contains(cfg.SyncBranches, branch) || contains(cfg.PreserveOnlyBranches, branch) || branch == "dev-test" {
		return problem("branch_not_candidate", "branch %q is not configured as a sync candidate", branch)
	}
	return nil
}

// plan reads both repositories of the pair and compares the histories of the sync branches.
func (w Workflow) plan(ctx context.Context, cfg config.Config, service string, pair config.Pair) (domain.RepositoryStatus, error) {
	if w.Reader == nil || w.Inspector == nil {
		return domain.RepositoryStatus{}, errors.New("workflow needs a repository reader and a history inspector")
	}
	scoped := cfg
	scoped.Services = []string{service}
	scoped.PairLabels = [][]string{{pair.Left.Label, pair.Right.Label}}
	report, err := app.Reporter{Reader: w.Reader, Inspector: w.Inspector}.Build(ctx, scoped, "plan")
	if err != nil {
		return domain.RepositoryStatus{}, fmt.Errorf("build plan: %w", err)
	}
	if len(report.Repositories) != 1 {
		return domain.RepositoryStatus{}, errors.New("expected exactly one repository in the scoped plan")
	}
	repository := report.Repositories[0]
	if len(repository.Errors) > 0 {
		return domain.RepositoryStatus{}, problem("plan_failed", "the plan could not be made: %s", strings.Join(repository.Errors, "; "))
	}
	return repository, nil
}

func branchOf(repository domain.RepositoryStatus, name string) (domain.BranchStatus, bool) {
	for _, branch := range repository.Branches {
		if branch.Name == name {
			return branch, true
		}
	}
	return domain.BranchStatus{}, false
}

func sides(repository domain.RepositoryStatus, branch domain.BranchStatus, targetLabel string) (target domain.RepositorySide, source domain.RepositorySide, targetSHA string, sourceSHA string, err error) {
	switch targetLabel {
	case repository.Left.Label:
		return repository.Left, repository.Right, branch.LeftSHA, branch.RightSHA, nil
	case repository.Right.Label:
		return repository.Right, repository.Left, branch.RightSHA, branch.LeftSHA, nil
	}
	return target, source, "", "", fmt.Errorf("unknown pull request target %q", targetLabel)
}

// Propose plans the sync and says what would be written. It writes nothing.
func (w Workflow) Propose(ctx context.Context, cfg config.Config, service string, branchName string, pairName string) (Proposal, error) {
	pair, err := ChoosePair(cfg, service, pairName)
	if err != nil {
		return Proposal{}, err
	}
	if err := checkBranch(cfg, branchName); err != nil {
		return Proposal{}, err
	}
	repository, err := w.plan(ctx, cfg, service, pair)
	if err != nil {
		return Proposal{}, err
	}
	branch, ok := branchOf(repository, branchName)
	if !ok {
		return Proposal{}, problem("branch_not_in_plan", "branch %q was not found in the plan", branchName)
	}
	proposal := Proposal{
		Service:    service,
		Pair:       pair.Name(),
		Branch:     branch,
		Repository: repository,
		Writes:     []ProposedWrite{},
		Phrase:     Phrase(service, branchName, pair.Name()),
	}
	if branch.Assessment != "clean_merge" {
		proposal.Blocked = fmt.Sprintf("%s (%s)", branch.Assessment, branch.Action)
		return proposal, nil
	}
	if w.Merge == nil {
		return Proposal{}, errors.New("workflow needs a merge-commit checker")
	}
	for _, label := range branch.PRTargets {
		target, source, targetSHA, sourceSHA, err := sides(repository, branch, label)
		if err != nil {
			return Proposal{}, err
		}
		allowed, err := w.Merge.MergeCommitAllowed(ctx, target.Owner, target.Repository)
		if err != nil {
			return Proposal{}, fmt.Errorf("read merge-commit setting for %s/%s: %w", target.Owner, target.Repository, err)
		}
		if !allowed && proposal.Blocked == "" {
			proposal.Blocked = fmt.Sprintf("%s/%s does not allow merge commits; no branch would be pushed", target.Owner, target.Repository)
		}
		proposal.Writes = append(proposal.Writes, ProposedWrite{
			TargetLabel:        label,
			TargetRepository:   target.Owner + "/" + target.Repository,
			SourceRepository:   source.Owner + "/" + source.Repository,
			CandidateBranch:    syncer.CandidateBranchName(label, source.Label, branchName, targetSHA, sourceSHA),
			BaseBranch:         branchName,
			SourceSHA:          sourceSHA,
			MergeCommitAllowed: allowed,
		})
	}
	return proposal, nil
}

// ApplyRequest is the person's confirmation of what Propose showed.
type ApplyRequest struct {
	Service string `json:"service"`
	Branch  string `json:"branch"`
	Pair    string `json:"pair"`
	// Confirmation must be exactly Phrase(service, branch, pair).
	Confirmation string `json:"confirmation"`
	// LeftSHA and RightSHA are the heads the person reviewed; if they moved, nothing is written.
	LeftSHA  string `json:"left_sha"`
	RightSHA string `json:"right_sha"`
}

// Apply pushes the new candidate branches and opens the pull requests, after planning again and
// checking that nothing moved since the person looked. It never merges, force-pushes or deletes.
func (w Workflow) Apply(ctx context.Context, cfg config.Config, request ApplyRequest) ([]syncer.Result, error) {
	pair, err := ChoosePair(cfg, request.Service, request.Pair)
	if err != nil {
		return nil, err
	}
	if err := checkBranch(cfg, request.Branch); err != nil {
		return nil, err
	}
	if request.Confirmation != Phrase(request.Service, request.Branch, pair.Name()) {
		return nil, problem("confirmation_mismatch", "type exactly %q to confirm", Phrase(request.Service, request.Branch, pair.Name()))
	}
	if w.Git == nil || w.PRs == nil || w.Merge == nil {
		return nil, errors.New("workflow needs a candidate pusher, a pull request manager and a merge-commit checker")
	}
	// Planned again after the confirmation: what is written is what is true now.
	repository, err := w.plan(ctx, cfg, request.Service, pair)
	if err != nil {
		return nil, err
	}
	branch, ok := branchOf(repository, request.Branch)
	if !ok {
		return nil, problem("branch_not_in_plan", "branch %q was not found in the plan", request.Branch)
	}
	if branch.Assessment != "clean_merge" {
		return nil, problem("not_clean", "no sync applied: %s (%s)", branch.Assessment, branch.Action)
	}
	if branch.LeftSHA != request.LeftSHA || branch.RightSHA != request.RightSHA {
		return nil, problem("plan_changed", "the branch heads changed after you reviewed them; review a fresh plan and try again")
	}
	executor := syncer.Executor{Git: w.Git, PRs: w.PRs, MergeChecker: w.Merge}
	return executor.Apply(ctx, repository, branch)
}
