package gitinspect

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BranchPush is the result of putting a branch of one repository into another.
type BranchPush struct {
	BranchName    string
	SHA           string
	AlreadyPushed bool
	// Ahead is how many commits the branch has that the base branch of the target lacks.
	Ahead int
}

// PushBranch creates `branchName` in the target repository at the commit it has in the source
// repository, so a pull request can bring it into `baseBranch`. It only ever creates: a branch of
// that name that already exists in the target is left alone (and is fine when it is the same
// commit), and nothing is force-pushed. The branch must share history with the target's base
// branch and have commits the base lacks, or there is nothing to propose.
func (i GitInspector) PushBranch(
	ctx context.Context,
	targetURL string,
	sourceURL string,
	branchName string,
	sourceSHA string,
	baseBranch string,
) (push BranchPush, returnErr error) {
	if branchName == "" || baseBranch == "" {
		return BranchPush{}, fmt.Errorf("branch name is empty")
	}
	if branchName == "dev-test" || branchName == baseBranch {
		return BranchPush{}, fmt.Errorf("%s cannot be propagated", branchName)
	}
	if !validCommitSHA(sourceSHA) {
		return BranchPush{}, fmt.Errorf("branch %q has an invalid commit SHA", branchName)
	}
	for _, name := range []string{branchName, baseBranch} {
		if _, err := i.run(ctx, "check-ref-format", "refs/heads/"+name); err != nil {
			return BranchPush{}, fmt.Errorf("branch name %q is invalid", name)
		}
	}

	directory, err := os.MkdirTemp("", "git-sync-propagate-")
	if err != nil {
		return BranchPush{}, fmt.Errorf("create temporary Git directory: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(directory); cleanupErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("remove temporary Git directory: %w", cleanupErr)
		}
	}()
	repositoryPath := filepath.Join(directory, "propagate.git")
	if _, err := i.run(ctx, "init", "--bare", "--quiet", repositoryPath); err != nil {
		return BranchPush{}, fmt.Errorf("initialize temporary Git repository: %w", err)
	}
	if err := i.fetch(ctx, repositoryPath, "source", sourceURL, []BranchHeads{{Name: branchName}}); err != nil {
		return BranchPush{}, fmt.Errorf("fetch source branch: %w", err)
	}
	if err := i.fetch(ctx, repositoryPath, "target", targetURL, []BranchHeads{{Name: baseBranch}}); err != nil {
		return BranchPush{}, fmt.Errorf("fetch target base branch %q: %w", baseBranch, err)
	}

	sourceRef := "refs/remotes/source/" + branchName
	baseRef := "refs/remotes/target/" + baseBranch
	actual, err := i.resolve(ctx, repositoryPath, sourceRef)
	if err != nil {
		return BranchPush{}, fmt.Errorf("resolve source branch: %w", err)
	}
	if actual != sourceSHA {
		return BranchPush{}, fmt.Errorf("branch %q changed since planning; rerun the plan", branchName)
	}
	if _, err := i.run(ctx, "-C", repositoryPath, "merge-base", baseRef, sourceRef); err != nil {
		if exitCode(err) == 1 {
			return BranchPush{}, fmt.Errorf("branch %q has no common ancestor with %q in the target; a pull request would carry the whole history", branchName, baseBranch)
		}
		return BranchPush{}, fmt.Errorf("find branch ancestry: %w", err)
	}
	counted, err := i.run(ctx, "-C", repositoryPath, "rev-list", "--count", baseRef+".."+sourceRef)
	if err != nil {
		return BranchPush{}, fmt.Errorf("count the commits to propose: %w", err)
	}
	ahead, err := strconv.Atoi(strings.TrimSpace(string(counted)))
	if err != nil {
		return BranchPush{}, fmt.Errorf("count the commits to propose: unexpected output")
	}
	if ahead == 0 {
		return BranchPush{}, fmt.Errorf("the target %q already contains everything in %q; nothing to propose", baseBranch, branchName)
	}

	remoteRef := "refs/heads/" + branchName
	existing, exists, err := i.remoteRefSHA(ctx, targetURL, remoteRef)
	if err != nil {
		return BranchPush{}, fmt.Errorf("check the target branch: %w", err)
	}
	if exists {
		if existing != sourceSHA {
			return BranchPush{}, fmt.Errorf("branch %q already exists in the target with different history; it was not changed", branchName)
		}
		return BranchPush{BranchName: branchName, SHA: sourceSHA, AlreadyPushed: true, Ahead: ahead}, nil
	}
	if err := i.verifyRemoteHead(ctx, sourceURL, branchName, sourceSHA); err != nil {
		return BranchPush{}, fmt.Errorf("source branch pre-push check: %w", err)
	}
	if _, err := i.run(ctx, "-C", repositoryPath, "update-ref", remoteRef, sourceSHA); err != nil {
		return BranchPush{}, fmt.Errorf("prepare the branch: %w", err)
	}
	if _, err := i.run(ctx, "-C", repositoryPath, "push", "--quiet", targetURL, remoteRef+":"+remoteRef); err != nil {
		return BranchPush{}, fmt.Errorf("push the new branch (no force was used): %w", err)
	}
	created := BranchPush{BranchName: branchName, SHA: sourceSHA, Ahead: ahead}
	pushed, found, err := i.remoteRefSHA(ctx, targetURL, remoteRef)
	if err != nil {
		return created, fmt.Errorf("verify the pushed branch: %w", err)
	}
	if !found || pushed != sourceSHA {
		return created, fmt.Errorf("the pushed branch is not what was sent; no pull request was created")
	}
	return created, nil
}
