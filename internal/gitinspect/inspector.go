package gitinspect

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type BranchHeads struct {
	Name     string
	LeftSHA  string
	RightSHA string
}

type Relation string

const (
	RelationSame        Relation = "same"
	RelationLeftBehind  Relation = "left_behind"
	RelationRightBehind Relation = "right_behind"
	RelationDiverged    Relation = "diverged"
	RelationUnrelated   Relation = "unrelated"
)

type Comparison struct {
	Relation      Relation
	MergeConflict bool
}

type Candidate struct {
	BranchName    string
	CommitSHA     string
	AlreadyPushed bool
}

type Inspector interface {
	Compare(ctx context.Context, leftURL string, rightURL string, heads []BranchHeads) (map[string]Comparison, error)
}

type GitInspector struct {
	Binary string
	Env    []string
}

func New() GitInspector {
	return GitInspector{Binary: "git"}
}

func (i GitInspector) Compare(ctx context.Context, leftURL string, rightURL string, heads []BranchHeads) (result map[string]Comparison, returnErr error) {
	comparisons := make(map[string]Comparison, len(heads))
	branchesToFetch := make([]BranchHeads, 0, len(heads))
	for _, branch := range heads {
		if branch.Name == "" {
			return nil, fmt.Errorf("branch name is empty")
		}
		if !validCommitSHA(branch.LeftSHA) || !validCommitSHA(branch.RightSHA) {
			return nil, fmt.Errorf("branch %q has an invalid commit SHA", branch.Name)
		}
		if branch.LeftSHA == branch.RightSHA {
			comparisons[branch.Name] = Comparison{Relation: RelationSame}
			continue
		}
		branchesToFetch = append(branchesToFetch, branch)
	}
	if len(branchesToFetch) == 0 {
		return comparisons, nil
	}

	directory, err := os.MkdirTemp("", "git-sync-history-")
	if err != nil {
		return nil, fmt.Errorf("create temporary Git directory: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(directory); cleanupErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("remove temporary Git directory: %w", cleanupErr)
		}
	}()

	repositoryPath := filepath.Join(directory, "history.git")
	if _, err := i.run(ctx, "init", "--bare", "--quiet", repositoryPath); err != nil {
		return nil, fmt.Errorf("initialize temporary Git repository: %w", err)
	}

	if err := i.fetch(ctx, repositoryPath, "left", leftURL, branchesToFetch); err != nil {
		return nil, fmt.Errorf("fetch left history: %w", err)
	}
	if err := i.fetch(ctx, repositoryPath, "right", rightURL, branchesToFetch); err != nil {
		return nil, fmt.Errorf("fetch right history: %w", err)
	}

	for _, branch := range branchesToFetch {
		leftRef := "refs/remotes/left/" + branch.Name
		rightRef := "refs/remotes/right/" + branch.Name
		leftSHA, err := i.resolve(ctx, repositoryPath, leftRef)
		if err != nil {
			return nil, fmt.Errorf("resolve left branch %q: %w", branch.Name, err)
		}
		rightSHA, err := i.resolve(ctx, repositoryPath, rightRef)
		if err != nil {
			return nil, fmt.Errorf("resolve right branch %q: %w", branch.Name, err)
		}
		if leftSHA != branch.LeftSHA || rightSHA != branch.RightSHA {
			return nil, fmt.Errorf("branch %q changed while it was being inspected; rerun the plan", branch.Name)
		}

		comparison, err := i.compareBranch(ctx, repositoryPath, leftRef, rightRef)
		if err != nil {
			return nil, fmt.Errorf("compare branch %q: %w", branch.Name, err)
		}
		comparisons[branch.Name] = comparison
	}

	return comparisons, nil
}

func (i GitInspector) PushCandidate(
	ctx context.Context,
	targetURL string,
	sourceURL string,
	branchName string,
	candidateBranch string,
	targetSHA string,
	sourceSHA string,
) (candidate Candidate, returnErr error) {
	if branchName == "" || candidateBranch == "" {
		return Candidate{}, fmt.Errorf("branch name is empty")
	}
	if branchName == "dev-test" {
		return Candidate{}, fmt.Errorf("dev-test is preserve-only and cannot receive a candidate branch")
	}
	if !validCommitSHA(targetSHA) || !validCommitSHA(sourceSHA) {
		return Candidate{}, fmt.Errorf("branch %q has an invalid commit SHA", branchName)
	}
	if _, err := i.run(ctx, "check-ref-format", "refs/heads/"+candidateBranch); err != nil {
		return Candidate{}, fmt.Errorf("candidate branch name is invalid")
	}

	directory, err := os.MkdirTemp("", "git-sync-push-")
	if err != nil {
		return Candidate{}, fmt.Errorf("create temporary Git directory: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(directory); cleanupErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("remove temporary Git directory: %w", cleanupErr)
		}
	}()

	repositoryPath := filepath.Join(directory, "candidate.git")
	if _, err := i.run(ctx, "init", "--bare", "--quiet", repositoryPath); err != nil {
		return Candidate{}, fmt.Errorf("initialize temporary Git repository: %w", err)
	}
	branch := []BranchHeads{{Name: branchName}}
	if err := i.fetch(ctx, repositoryPath, "target", targetURL, branch); err != nil {
		return Candidate{}, fmt.Errorf("fetch target history: %w", err)
	}
	if err := i.fetch(ctx, repositoryPath, "source", sourceURL, branch); err != nil {
		return Candidate{}, fmt.Errorf("fetch source history: %w", err)
	}

	targetRef := "refs/remotes/target/" + branchName
	sourceRef := "refs/remotes/source/" + branchName
	actualTargetSHA, err := i.resolve(ctx, repositoryPath, targetRef)
	if err != nil {
		return Candidate{}, fmt.Errorf("resolve target branch: %w", err)
	}
	actualSourceSHA, err := i.resolve(ctx, repositoryPath, sourceRef)
	if err != nil {
		return Candidate{}, fmt.Errorf("resolve source branch: %w", err)
	}
	if actualTargetSHA != targetSHA || actualSourceSHA != sourceSHA {
		return Candidate{}, fmt.Errorf("branch %q changed since planning; rerun the plan", branchName)
	}
	if targetSHA == sourceSHA {
		return Candidate{}, fmt.Errorf("branch %q is already synchronized", branchName)
	}

	_, err = i.run(ctx, "-C", repositoryPath, "merge-base", targetRef, sourceRef)
	if err != nil {
		if exitCode(err) == 1 {
			return Candidate{}, fmt.Errorf("branch %q histories have no common ancestor", branchName)
		}
		return Candidate{}, fmt.Errorf("find branch ancestry: %w", err)
	}
	targetIsAncestor, err := i.isAncestor(ctx, repositoryPath, targetRef, sourceRef)
	if err != nil {
		return Candidate{}, fmt.Errorf("check target ancestry: %w", err)
	}
	sourceIsAncestor, err := i.isAncestor(ctx, repositoryPath, sourceRef, targetRef)
	if err != nil {
		return Candidate{}, fmt.Errorf("check source ancestry: %w", err)
	}

	candidateSHA := sourceSHA
	if sourceIsAncestor {
		return Candidate{}, fmt.Errorf("target already contains the source history; rerun the plan")
	}
	if !targetIsAncestor {
		mergeTree, err := i.run(ctx, "-C", repositoryPath, "merge-tree", "--write-tree", targetRef, sourceRef)
		if err != nil {
			if exitCode(err) == 1 {
				return Candidate{}, fmt.Errorf("branch %q has merge conflicts", branchName)
			}
			return Candidate{}, fmt.Errorf("build merge tree: %w", err)
		}
		fields := strings.Fields(string(mergeTree))
		if len(fields) == 0 || !validCommitSHA(fields[0]) {
			return Candidate{}, fmt.Errorf("git returned an invalid merge tree for branch %q", branchName)
		}
		treeSHA := fields[0]
		mergeDate, err := i.mergeDate(ctx, repositoryPath, targetSHA, sourceSHA)
		if err != nil {
			return Candidate{}, fmt.Errorf("determine deterministic merge timestamp: %w", err)
		}
		message := "chore(sync): combine " + branchName + " histories"
		commitEnv := append([]string(nil), i.Env...)
		commitEnv = append(commitEnv,
			"GIT_AUTHOR_DATE="+mergeDate,
			"GIT_COMMITTER_DATE="+mergeDate,
		)
		commitOutput, err := i.runWithEnv(ctx, commitEnv, "-C", repositoryPath,
			"commit-tree", treeSHA, "-p", targetSHA, "-p", sourceSHA, "-m", message)
		if err != nil {
			return Candidate{}, fmt.Errorf("create preserving merge commit (check git user.name and user.email): %w", err)
		}
		candidateSHA = strings.TrimSpace(string(commitOutput))
		if !validCommitSHA(candidateSHA) {
			return Candidate{}, fmt.Errorf("git returned an invalid candidate commit for branch %q", branchName)
		}
	}

	localRef := "refs/heads/" + candidateBranch
	remoteRef := "refs/heads/" + candidateBranch
	if _, err := i.run(ctx, "-C", repositoryPath, "update-ref", localRef, candidateSHA); err != nil {
		return Candidate{}, fmt.Errorf("prepare candidate branch: %w", err)
	}
	if err := i.verifyRemoteHead(ctx, targetURL, branchName, targetSHA); err != nil {
		return Candidate{}, fmt.Errorf("target branch pre-push check: %w", err)
	}
	if err := i.verifyRemoteHead(ctx, sourceURL, branchName, sourceSHA); err != nil {
		return Candidate{}, fmt.Errorf("source branch pre-push check: %w", err)
	}

	remoteCandidateSHA, exists, err := i.remoteRefSHA(ctx, targetURL, remoteRef)
	if err != nil {
		return Candidate{}, fmt.Errorf("check candidate branch: %w", err)
	}
	if exists {
		if remoteCandidateSHA != candidateSHA {
			return Candidate{}, fmt.Errorf("candidate branch %q already exists with different history; it was not changed", candidateBranch)
		}
		return Candidate{BranchName: candidateBranch, CommitSHA: candidateSHA, AlreadyPushed: true}, nil
	}

	if _, err := i.run(ctx, "-C", repositoryPath, "push", "--quiet", targetURL, localRef+":"+remoteRef); err != nil {
		return Candidate{}, fmt.Errorf("push new candidate branch (no force was used): %w", err)
	}
	createdCandidate := Candidate{BranchName: candidateBranch, CommitSHA: candidateSHA}
	pushedSHA, exists, err := i.remoteRefSHA(ctx, targetURL, remoteRef)
	if err != nil {
		return createdCandidate, fmt.Errorf("verify pushed candidate branch: %w", err)
	}
	if !exists || pushedSHA != candidateSHA {
		return createdCandidate, fmt.Errorf("candidate branch changed after push; no pull request was created")
	}
	if err := i.verifyRemoteHead(ctx, targetURL, branchName, targetSHA); err != nil {
		return createdCandidate, fmt.Errorf("target branch changed after push; candidate remains without a PR")
	}
	if err := i.verifyRemoteHead(ctx, sourceURL, branchName, sourceSHA); err != nil {
		return createdCandidate, fmt.Errorf("source branch changed after push; candidate remains without a PR")
	}

	return createdCandidate, nil
}

func (i GitInspector) fetch(ctx context.Context, repositoryPath string, side string, url string, branches []BranchHeads) error {
	args := []string{"-C", repositoryPath, "fetch", "--quiet", "--no-tags", url}
	for _, branch := range branches {
		ref := "refs/heads/" + branch.Name
		if _, err := i.run(ctx, "check-ref-format", ref); err != nil {
			return fmt.Errorf("invalid branch name")
		}
		args = append(args, "+"+ref+":refs/remotes/"+side+"/"+branch.Name)
	}
	if _, err := i.run(ctx, args...); err != nil {
		return fmt.Errorf("git fetch failed: %w", err)
	}
	return nil
}

func (i GitInspector) resolve(ctx context.Context, repositoryPath string, ref string) (string, error) {
	output, err := i.run(ctx, "-C", repositoryPath, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func (i GitInspector) compareBranch(ctx context.Context, repositoryPath string, leftRef string, rightRef string) (Comparison, error) {
	_, err := i.run(ctx, "-C", repositoryPath, "merge-base", leftRef, rightRef)
	if err != nil {
		if exitCode(err) == 1 {
			return Comparison{Relation: RelationUnrelated}, nil
		}
		return Comparison{}, err
	}

	leftIsAncestor, err := i.isAncestor(ctx, repositoryPath, leftRef, rightRef)
	if err != nil {
		return Comparison{}, err
	}
	rightIsAncestor, err := i.isAncestor(ctx, repositoryPath, rightRef, leftRef)
	if err != nil {
		return Comparison{}, err
	}

	relation := RelationDiverged
	if leftIsAncestor {
		relation = RelationLeftBehind
	} else if rightIsAncestor {
		relation = RelationRightBehind
	}

	_, err = i.run(ctx, "-C", repositoryPath, "merge-tree", "--write-tree", leftRef, rightRef)
	if err == nil {
		return Comparison{Relation: relation}, nil
	}
	if exitCode(err) == 1 {
		return Comparison{Relation: relation, MergeConflict: true}, nil
	}
	return Comparison{}, err
}

func (i GitInspector) isAncestor(ctx context.Context, repositoryPath string, ancestor string, descendant string) (bool, error) {
	_, err := i.run(ctx, "-C", repositoryPath, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

func (i GitInspector) mergeDate(ctx context.Context, repositoryPath string, firstParent string, secondParent string) (string, error) {
	firstDate, err := i.commitTimestamp(ctx, repositoryPath, firstParent)
	if err != nil {
		return "", err
	}
	secondDate, err := i.commitTimestamp(ctx, repositoryPath, secondParent)
	if err != nil {
		return "", err
	}
	if secondDate > firstDate {
		firstDate = secondDate
	}
	return "@" + strconv.FormatInt(firstDate+1, 10) + " +0000", nil
}

func (i GitInspector) commitTimestamp(ctx context.Context, repositoryPath string, commit string) (int64, error) {
	output, err := i.run(ctx, "-C", repositoryPath, "show", "-s", "--format=%ct", commit)
	if err != nil {
		return 0, err
	}
	timestamp, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse commit timestamp: %w", err)
	}
	return timestamp, nil
}

func (i GitInspector) remoteRefSHA(ctx context.Context, remoteURL string, ref string) (string, bool, error) {
	output, err := i.run(ctx, "ls-remote", "--heads", "--refs", remoteURL, ref)
	if err != nil {
		return "", false, err
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return "", false, nil
	}
	if len(fields) != 2 || fields[1] != ref || !validCommitSHA(fields[0]) {
		return "", false, fmt.Errorf("git returned an invalid remote ref response")
	}
	return fields[0], true, nil
}

func (i GitInspector) verifyRemoteHead(ctx context.Context, remoteURL string, branch string, expectedSHA string) error {
	ref := "refs/heads/" + branch
	actualSHA, exists, err := i.remoteRefSHA(ctx, remoteURL, ref)
	if err != nil {
		return err
	}
	if !exists || actualSHA != expectedSHA {
		return fmt.Errorf("branch %q moved since planning; no candidate was pushed", branch)
	}
	return nil
}

func (i GitInspector) run(ctx context.Context, args ...string) ([]byte, error) {
	return i.runWithEnv(ctx, nil, args...)
}

func (i GitInspector) runWithEnv(ctx context.Context, extraEnv []string, args ...string) ([]byte, error) {
	binary := i.Binary
	if binary == "" {
		binary = "git"
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = append(os.Environ(), i.Env...)
	command.Env = append(command.Env, extraEnv...)
	output, err := command.Output()
	if err == nil {
		return output, nil
	}

	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return nil, gitCommandError{code: exitError.ExitCode()}
	}
	return nil, err
}

type gitCommandError struct {
	code int
}

func (e gitCommandError) Error() string {
	return fmt.Sprintf("Git command failed with exit code %d", e.code)
}

func exitCode(err error) int {
	var commandError gitCommandError
	if errors.As(err, &commandError) {
		return commandError.code
	}
	return -1
}

func validCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
