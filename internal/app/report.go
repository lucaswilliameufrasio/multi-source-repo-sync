package app

import (
	"context"
	"fmt"
	"sort"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/domain"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
)

const (
	modeStatus = "status"
	modePlan   = "plan"
)

type Reporter struct {
	Reader    github.RepositoryReader
	Inspector gitinspect.Inspector
}

func (r Reporter) Build(ctx context.Context, cfg config.Config, mode string) (domain.Report, error) {
	if mode != modeStatus && mode != modePlan {
		return domain.Report{}, fmt.Errorf("unsupported report mode %q", mode)
	}
	if r.Reader == nil {
		return domain.Report{}, fmt.Errorf("repository reader is required")
	}
	if mode == modePlan && r.Inspector == nil {
		return domain.Report{}, fmt.Errorf("history inspector is required for plan mode")
	}

	report := domain.Report{
		Mode:         mode,
		Repositories: make([]domain.RepositoryStatus, 0, len(cfg.Services)),
	}
	// A repository takes part in several pairs but is read from GitHub once.
	reads := make(map[string]sideRead)
	for _, service := range cfg.Services {
		for _, pair := range cfg.PairsFor(service) {
			repository := r.readRepository(ctx, cfg, pair, service, reads)
			if mode == modePlan {
				r.planRepository(ctx, cfg, &repository)
			}
			report.Repositories = append(report.Repositories, repository)
		}
	}

	return report, nil
}

// sideRead is what reading one repository gave: a snapshot or the reason it failed.
type sideRead struct {
	snapshot github.Snapshot
	err      error
}

func (r Reporter) readSide(ctx context.Context, organization config.Organization, service string, reads map[string]sideRead) domain.RepositorySide {
	key := organization.Owner + "/" + service
	read, done := reads[key]
	if !done {
		snapshot, err := r.Reader.ReadRepository(ctx, organization.Owner, service, organization.Label)
		read = sideRead{snapshot: snapshot, err: err}
		reads[key] = read
	}
	if read.err != nil {
		return domain.RepositorySide{Label: organization.Label, Owner: organization.Owner, Repository: service, Error: read.err.Error()}
	}
	return makeSide(organization.Label, organization.Owner, service, read.snapshot)
}

func (r Reporter) readRepository(ctx context.Context, cfg config.Config, pair config.Pair, service string, reads map[string]sideRead) domain.RepositoryStatus {
	left := r.readSide(ctx, pair.Left, service, reads)
	right := r.readSide(ctx, pair.Right, service, reads)

	repository := domain.RepositoryStatus{
		Service: service,
		Pair:    pair.Name(),
		Left:    left,
		Right:   right,
	}
	if left.Error != "" {
		repository.Errors = append(repository.Errors, left.Label+": "+left.Error)
	}
	if right.Error != "" {
		repository.Errors = append(repository.Errors, right.Label+": "+right.Error)
	}
	repository.Branches = buildBranchStatuses(cfg, left, right, modeStatus)
	return repository
}

func makeSide(label string, owner string, repository string, snapshot github.Snapshot) domain.RepositorySide {
	branchNames := make([]string, 0, len(snapshot.Branches))
	for name := range snapshot.Branches {
		branchNames = append(branchNames, name)
	}
	sort.Strings(branchNames)

	branches := make([]domain.BranchRef, 0, len(branchNames))
	for _, name := range branchNames {
		branches = append(branches, domain.BranchRef{Name: name, SHA: snapshot.Branches[name]})
	}

	pullRequests := append([]domain.PullRequest(nil), snapshot.OpenPullRequests...)
	sort.Slice(pullRequests, func(i int, j int) bool {
		return pullRequests[i].Number < pullRequests[j].Number
	})

	return domain.RepositorySide{
		Label:            label,
		Owner:            owner,
		Repository:       repository,
		Branches:         branches,
		OpenPullRequests: pullRequests,
	}
}

func (r Reporter) planRepository(ctx context.Context, cfg config.Config, repository *domain.RepositoryStatus) {
	if repository.Left.Error != "" || repository.Right.Error != "" {
		repository.Branches = buildBranchStatuses(cfg, repository.Left, repository.Right, modePlan)
		return
	}

	leftBranches := branchMap(repository.Left.Branches)
	rightBranches := branchMap(repository.Right.Branches)
	var heads []gitinspect.BranchHeads
	for _, name := range cfg.SyncBranches {
		leftSHA, leftOK := leftBranches[name]
		rightSHA, rightOK := rightBranches[name]
		if leftOK && rightOK && leftSHA != rightSHA {
			heads = append(heads, gitinspect.BranchHeads{
				Name:     name,
				LeftSHA:  leftSHA,
				RightSHA: rightSHA,
			})
		}
	}

	comparisons := make(map[string]gitinspect.Comparison)
	var inspectErr error
	if len(heads) > 0 {
		comparisons, inspectErr = r.Inspector.Compare(
			ctx,
			github.RepositoryURL(repository.Left.Owner, repository.Left.Repository),
			github.RepositoryURL(repository.Right.Owner, repository.Right.Repository),
			heads,
		)
		if inspectErr != nil {
			repository.Errors = append(repository.Errors, "history inspection: "+inspectErr.Error())
		}
	}

	repository.Branches = buildBranchStatuses(cfg, repository.Left, repository.Right, modePlan)
	for index := range repository.Branches {
		branch := &repository.Branches[index]
		if branch.Policy != "sync_candidate" || branch.Assessment != "different_heads" {
			continue
		}
		if inspectErr != nil {
			branch.Assessment = "history_unavailable"
			branch.Action = "stop_and_retry"
			branch.Reason = "Git could not establish ancestry; no remote refs or pull requests were created."
			continue
		}
		comparison, ok := comparisons[branch.Name]
		if !ok {
			branch.Assessment = "history_unavailable"
			branch.Action = "stop_and_retry"
			branch.Reason = "No history comparison was returned; no remote refs or pull requests were created."
			repository.Errors = append(repository.Errors, "history inspection returned no comparison for branch "+branch.Name)
			continue
		}
		renderComparison(branch, comparison, repository.Left, repository.Right)
	}
}

func buildBranchStatuses(cfg config.Config, leftSide domain.RepositorySide, rightSide domain.RepositorySide, mode string) []domain.BranchStatus {
	leftBranches := branchMap(leftSide.Branches)
	rightBranches := branchMap(rightSide.Branches)
	names := make(map[string]struct{}, len(leftBranches)+len(rightBranches)+len(cfg.SyncBranches)+len(cfg.PreserveOnlyBranches))
	for name := range leftBranches {
		names[name] = struct{}{}
	}
	for name := range rightBranches {
		names[name] = struct{}{}
	}
	for _, name := range cfg.SyncBranches {
		names[name] = struct{}{}
	}
	for _, name := range cfg.PreserveOnlyBranches {
		names[name] = struct{}{}
	}

	orderedNames := make([]string, 0, len(names))
	for name := range names {
		orderedNames = append(orderedNames, name)
	}
	sort.Strings(orderedNames)

	syncable := make(map[string]struct{}, len(cfg.SyncBranches))
	for _, name := range cfg.SyncBranches {
		syncable[name] = struct{}{}
	}
	preserveOnly := make(map[string]struct{}, len(cfg.PreserveOnlyBranches))
	for _, name := range cfg.PreserveOnlyBranches {
		preserveOnly[name] = struct{}{}
	}

	statuses := make([]domain.BranchStatus, 0, len(orderedNames))
	for _, name := range orderedNames {
		status := domain.BranchStatus{Name: name, Policy: "inventory_only"}
		if _, ok := syncable[name]; ok {
			status.Policy = "sync_candidate"
		}
		if _, ok := preserveOnly[name]; ok {
			status.Policy = "preserve_only"
		}
		status.LeftSHA = leftBranches[name]
		status.RightSHA = rightBranches[name]

		if status.Policy == "preserve_only" {
			status.Assessment = "preserved"
			status.Action = "never_sync_or_delete"
			status.Reason = "This branch is inventoried only and is excluded from all synchronization plans."
			statuses = append(statuses, status)
			continue
		}

		if leftSide.Error != "" || rightSide.Error != "" {
			status.Assessment = "repository_unavailable"
			status.Action = "do_not_plan"
			statuses = append(statuses, status)
			continue
		}

		leftOK := status.LeftSHA != ""
		rightOK := status.RightSHA != ""
		switch {
		case !leftOK && !rightOK:
			status.Assessment = "missing_on_both"
			status.Action = "no_action"
		case !leftOK:
			status.Assessment = "missing_on_" + leftSide.Label
			status.Action = "preserve_existing_branch"
			status.Reason = "The branch exists only in " + rightSide.Label + "; no counterpart will be created automatically."
		case !rightOK:
			status.Assessment = "missing_on_" + rightSide.Label
			status.Action = "preserve_existing_branch"
			status.Reason = "The branch exists only in " + leftSide.Label + "; no counterpart will be created automatically."
		case status.LeftSHA == status.RightSHA:
			status.Assessment = "in_sync"
			status.Action = "no_action"
		case status.Policy != "sync_candidate":
			status.Assessment = "different_heads"
			status.Action = "report_only"
			status.Reason = "This branch is inventoried but is outside the configured synchronization candidates."
		case mode == modeStatus:
			status.Assessment = "different_heads"
			status.Action = "ancestry_not_checked"
			status.Reason = "Run plan to inspect commit ancestry and mergeability."
		default:
			status.Assessment = "different_heads"
			status.Action = "inspect_history"
		}
		statuses = append(statuses, status)
	}

	return statuses
}

func branchMap(branches []domain.BranchRef) map[string]string {
	result := make(map[string]string, len(branches))
	for _, branch := range branches {
		result[branch.Name] = branch.SHA
	}
	return result
}

func renderComparison(branch *domain.BranchStatus, comparison gitinspect.Comparison, left domain.RepositorySide, right domain.RepositorySide) {
	switch {
	case comparison.Relation == gitinspect.RelationUnrelated:
		branch.Assessment = "unrelated_histories"
		branch.Action = "stop_for_manual_review"
		branch.Reason = "No common ancestor was found; no remote refs or pull requests were created."
	case comparison.MergeConflict:
		branch.Assessment = "merge_conflict"
		branch.Action = "stop_for_conflict_resolution"
		branch.Reason = "The Git merge tree has conflicts; no remote refs or pull requests were created."
	default:
		branch.Assessment = "clean_merge"
		branch.Action = "prepare_pull_request_candidates"
		branch.Reason = "The histories can be combined without conflicts. This read-only plan did not create branches or pull requests."
		switch comparison.Relation {
		case gitinspect.RelationLeftBehind:
			branch.PRTargets = []string{left.Label}
		case gitinspect.RelationRightBehind:
			branch.PRTargets = []string{right.Label}
		case gitinspect.RelationDiverged:
			branch.PRTargets = []string{left.Label, right.Label}
		}
	}
}
