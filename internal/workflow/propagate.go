package workflow

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
)

// BranchPusher puts a branch of one repository into another (gitinspect.GitInspector does).
type BranchPusher interface {
	PushBranch(ctx context.Context, targetURL string, sourceURL string, branchName string, sourceSHA string, baseBranch string) (gitinspect.BranchPush, error)
}

var featureBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/_.-]{0,199}$`)

// Target states of a propagation.
const (
	StateNew              = "new"               // the branch does not exist there: it would be created
	StateAlreadyThere     = "already_there"     // the same commit is already there: only the pull request is missing
	StateDifferentHistory = "different_history" // a branch of that name has other commits there: left alone
	StateNoBase           = "no_base"           // the target has no base branch to propose into
	StateMergeCommitsOff  = "merge_commits_off" // the target does not allow merge commits
)

// PropagateTarget is what would happen in one organization.
type PropagateTarget struct {
	Label       string `json:"label"`
	Repository  string `json:"repository"`
	State       string `json:"state"`
	OpenPullURL string `json:"open_pull_request,omitempty"`
}

// PropagateProposal is what putting a branch into the other copies would do, with nothing done.
type PropagateProposal struct {
	Service   string            `json:"service"`
	Branch    string            `json:"branch"`
	Source    string            `json:"source"`
	SourceSHA string            `json:"source_sha"`
	Base      string            `json:"base"`
	Targets   []PropagateTarget `json:"targets"`
	Phrase    string            `json:"phrase"`
}

// PropagatePhrase is what the person types to allow it.
func PropagatePhrase(service string, branch string, source string, targets []string) string {
	return "propagate " + service + " " + branch + " from " + source + " to " + strings.Join(targets, ",")
}

// PropagateRequest is the person's confirmation of a proposal.
type PropagateRequest struct {
	Service      string   `json:"service"`
	Branch       string   `json:"branch"`
	Source       string   `json:"source"`
	Targets      []string `json:"targets"`
	Base         string   `json:"base"`
	Confirmation string   `json:"confirmation"`
	// SourceSHA is the commit the person reviewed; if the branch moved, nothing is written.
	SourceSHA string `json:"source_sha"`
}

// PropagateResult is what happened in one organization.
type PropagateResult struct {
	Label       string `json:"label"`
	Repository  string `json:"repository"`
	Status      string `json:"status"` // created, already_there, skipped, failed
	PullRequest string `json:"pull_request_url,omitempty"`
	PRCreated   bool   `json:"pull_request_created"`
	Detail      string `json:"detail,omitempty"`
}

func (w Workflow) read(ctx context.Context, organization config.Organization, service string) (github.Snapshot, error) {
	snapshot, err := w.Reader.ReadRepository(ctx, organization.Owner, service, organization.Label)
	if err != nil {
		return github.Snapshot{}, fmt.Errorf("read %s/%s: %w", organization.Owner, service, err)
	}
	return snapshot, nil
}

// plannedPropagation checks the request and reads every repository once.
func (w Workflow) plannedPropagation(ctx context.Context, cfg config.Config, service string, branch string, sourceLabel string, to []string, base string) (PropagateProposal, error) {
	if w.Reader == nil {
		return PropagateProposal{}, errors.New("workflow needs a repository reader")
	}
	if !contains(cfg.Services, service) {
		return PropagateProposal{}, problem("unknown_service", "service %q is not in the configured allowlist", service)
	}
	if !featureBranch.MatchString(branch) || strings.Contains(branch, "..") || strings.Contains(branch, "//") ||
		strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".lock") || strings.HasPrefix(branch, "sync/") {
		return PropagateProposal{}, problem("branch_invalid", "branch %q cannot be propagated (invalid name, or reserved for synchronization)", branch)
	}
	if contains(cfg.SyncBranches, branch) || contains(cfg.PreserveOnlyBranches, branch) || branch == "dev-test" {
		return PropagateProposal{}, problem("branch_reserved", "branch %q is a synchronization or preserved branch; synchronize it with sync instead", branch)
	}
	if base == "" {
		base = cfg.SyncBranches[0]
	}
	if !contains(cfg.SyncBranches, base) {
		return PropagateProposal{}, problem("base_not_allowed", "the pull requests can only go into one of: %s", strings.Join(cfg.SyncBranches, ", "))
	}
	source, ok := cfg.Organizations.Find(sourceLabel)
	if !ok {
		return PropagateProposal{}, problem("unknown_organization", "organization %q is not in the manifest", sourceLabel)
	}
	if !cfg.HasService(source.Label, service) {
		return PropagateProposal{}, problem("service_not_in_source", "%s has no copy of %q", source.Label, service)
	}

	var targets []config.Organization
	if len(to) == 0 {
		for _, pair := range cfg.PairsFor(service) {
			switch source.Label {
			case pair.Left.Label:
				targets = append(targets, pair.Right)
			case pair.Right.Label:
				targets = append(targets, pair.Left)
			}
		}
	} else {
		for _, label := range to {
			target, ok := cfg.Organizations.Find(label)
			if !ok {
				return PropagateProposal{}, problem("unknown_organization", "organization %q is not in the manifest", label)
			}
			if target.Label == source.Label {
				return PropagateProposal{}, problem("same_organization", "the source %q cannot also be a target", label)
			}
			if _, configured := cfg.Pair(source.Label + "/" + target.Label); !configured || !cfg.HasService(target.Label, service) {
				return PropagateProposal{}, problem("service_not_in_pair", "%q is not a target of %s for %q (not a configured pair, or no copy there)", label, source.Label, service)
			}
			targets = append(targets, target)
		}
	}
	if len(targets) == 0 {
		return PropagateProposal{}, problem("no_targets", "there is no other organization with a copy of %q to receive it", service)
	}

	snapshot, err := w.read(ctx, source, service)
	if err != nil {
		return PropagateProposal{}, err
	}
	sourceSHA := snapshot.Branches[branch]
	if sourceSHA == "" {
		return PropagateProposal{}, problem("branch_not_in_source", "%s/%s has no branch %q", source.Owner, service, branch)
	}

	proposal := PropagateProposal{Service: service, Branch: branch, Source: source.Label, SourceSHA: sourceSHA, Base: base, Targets: []PropagateTarget{}}
	labels := make([]string, 0, len(targets))
	for _, target := range targets {
		labels = append(labels, target.Label)
		view, err := w.read(ctx, target, service)
		if err != nil {
			return PropagateProposal{}, err
		}
		entry := PropagateTarget{Label: target.Label, Repository: target.Owner + "/" + service}
		switch existing := view.Branches[branch]; {
		case view.Branches[base] == "":
			entry.State = StateNoBase
		case existing == sourceSHA:
			entry.State = StateAlreadyThere
		case existing != "":
			entry.State = StateDifferentHistory
		default:
			entry.State = StateNew
		}
		if entry.State == StateNew || entry.State == StateAlreadyThere {
			if w.Merge != nil {
				allowed, err := w.Merge.MergeCommitAllowed(ctx, target.Owner, service)
				if err != nil {
					return PropagateProposal{}, fmt.Errorf("read merge-commit setting for %s/%s: %w", target.Owner, service, err)
				}
				if !allowed {
					entry.State = StateMergeCommitsOff
				}
			}
		}
		for _, pull := range view.OpenPullRequests {
			if pull.HeadBranch == branch && pull.BaseBranch == base {
				entry.OpenPullURL = pull.URL
			}
		}
		proposal.Targets = append(proposal.Targets, entry)
	}
	proposal.Phrase = PropagatePhrase(service, branch, source.Label, labels)
	return proposal, nil
}

// ProposePropagation says what putting `branch` of `sourceLabel` into the other copies would do.
// It writes nothing. `to` empty means every other organization with a copy.
func (w Workflow) ProposePropagation(ctx context.Context, cfg config.Config, service string, branch string, sourceLabel string, to []string, base string) (PropagateProposal, error) {
	return w.plannedPropagation(ctx, cfg, service, branch, sourceLabel, to, base)
}

// ApplyPropagation creates the branch in each target that does not have it and opens a pull request
// into the base branch, after planning again and checking that the source did not move. A target
// that cannot take it (other history under that name, no base, merge commits off) is skipped and
// said so; the others still go ahead. It never merges, force-pushes or deletes.
func (w Workflow) ApplyPropagation(ctx context.Context, cfg config.Config, request PropagateRequest) ([]PropagateResult, error) {
	if w.Branches == nil || w.PRs == nil {
		return nil, errors.New("workflow needs a branch pusher and a pull request manager")
	}
	fresh, err := w.plannedPropagation(ctx, cfg, request.Service, request.Branch, request.Source, request.Targets, request.Base)
	if err != nil {
		return nil, err
	}
	if request.Confirmation != fresh.Phrase {
		return nil, problem("confirmation_mismatch", "type exactly %q to confirm", fresh.Phrase)
	}
	if fresh.SourceSHA != request.SourceSHA {
		return nil, problem("plan_changed", "the branch moved after you reviewed it; review a fresh plan and try again")
	}
	source, _ := cfg.Organizations.Find(fresh.Source)
	results := make([]PropagateResult, 0, len(fresh.Targets))
	for _, target := range fresh.Targets {
		result := PropagateResult{Label: target.Label, Repository: target.Repository}
		switch target.State {
		case StateDifferentHistory:
			result.Status, result.Detail = "skipped", "a branch with this name already exists there with other commits; it was not touched"
		case StateNoBase:
			result.Status, result.Detail = "skipped", fmt.Sprintf("there is no %q branch to propose into", fresh.Base)
		case StateMergeCommitsOff:
			result.Status, result.Detail = "skipped", "the repository does not allow merge commits, which the pull request needs to keep the history"
		default:
			organization, _ := cfg.Organizations.Find(target.Label)
			pushed, err := w.Branches.PushBranch(ctx,
				github.RepositoryURL(organization.Owner, fresh.Service), github.RepositoryURL(source.Owner, fresh.Service),
				fresh.Branch, fresh.SourceSHA, fresh.Base)
			if err != nil {
				result.Status, result.Detail = "failed", err.Error()
				break
			}
			result.Status = "created"
			if pushed.AlreadyPushed {
				result.Status = "already_there"
			}
			title, body := propagationText(fresh, source, organization, pushed.Ahead)
			pull, err := w.PRs.EnsurePullRequest(ctx, organization.Owner, fresh.Service, fresh.Branch, fresh.Base, title, body)
			if err != nil {
				result.Detail = fmt.Sprintf("the branch is there but the pull request failed: %v", err)
				break
			}
			result.PullRequest, result.PRCreated = pull.URL, pull.Created
		}
		results = append(results, result)
	}
	return results, nil
}

func propagationText(proposal PropagateProposal, source config.Organization, target config.Organization, ahead int) (string, string) {
	title := fmt.Sprintf("Bring %s from %s into %s", proposal.Branch, source.Owner, proposal.Base)
	body := strings.Join([]string{
		fmt.Sprintf("The branch %s of %s/%s (%d commit(s) the %s branch here lacks), put here as it is by multi-source-repo-sync.", proposal.Branch, source.Owner, proposal.Service, ahead, proposal.Base),
		"",
		"Source: " + source.Owner + "/" + proposal.Service + " " + proposal.Branch + " at " + proposal.SourceSHA,
		"",
		"If merging this PR, use the GitHub merge-commit method. Do not squash or rebase: the original commit identities must remain reachable.",
		"No automatic merge was requested.",
	}, "\n")
	return title, body
}
