package gitinspect

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareRepositoryHistories(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		wantRelation Relation
		wantConflict bool
	}{
		{name: "same", wantRelation: RelationSame},
		{name: "Acme behind", wantRelation: RelationLeftBehind},
		{name: "Globex behind", wantRelation: RelationRightBehind},
		{name: "clean divergence", wantRelation: RelationDiverged},
		{name: "conflicting divergence", wantRelation: RelationDiverged, wantConflict: true},
		{name: "unrelated histories", wantRelation: RelationUnrelated},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			tixNow, globex, leftSHA, rightSHA := historyPair(t, testCase.name)
			inspector := New()
			got, err := inspector.Compare(context.Background(), tixNow, globex, []BranchHeads{{
				Name:     "develop",
				LeftSHA:  leftSHA,
				RightSHA: rightSHA,
			}})
			if err != nil {
				t.Fatalf("Compare() error = %v", err)
			}
			comparison, ok := got["develop"]
			if !ok {
				t.Fatal("Compare() did not return develop")
			}
			if comparison.Relation != testCase.wantRelation {
				t.Fatalf("relation = %q, want %q", comparison.Relation, testCase.wantRelation)
			}
			if comparison.MergeConflict != testCase.wantConflict {
				t.Fatalf("merge conflict = %t, want %t", comparison.MergeConflict, testCase.wantConflict)
			}
		})
	}
}

func TestCompareRejectsStaleExpectedHead(t *testing.T) {
	t.Parallel()

	tixNow, globex, _, globexSHA := historyPair(t, "Acme behind")
	inspector := New()
	_, err := inspector.Compare(context.Background(), tixNow, globex, []BranchHeads{{
		Name:     "develop",
		LeftSHA:  strings.Repeat("0", 40),
		RightSHA: globexSHA,
	}})
	if err == nil || !strings.Contains(err.Error(), "changed while it was being inspected") {
		t.Fatalf("Compare() error = %v, want stale-head error", err)
	}
}

func TestCompareRejectsInvalidSHA(t *testing.T) {
	t.Parallel()

	_, err := New().Compare(context.Background(), "unused", "unused", []BranchHeads{{
		Name:     "develop",
		LeftSHA:  "not-a-sha",
		RightSHA: strings.Repeat("a", 40),
	}})
	if err == nil || !strings.Contains(err.Error(), "invalid commit SHA") {
		t.Fatalf("Compare() error = %v, want invalid SHA error", err)
	}
}

func TestPushCandidateCreatesNewBranchWithoutMovingBaseAndIsIdempotent(t *testing.T) {
	t.Parallel()

	tixRemote, globexRemote, targetSHA, sourceSHA, env := pushHistoryPair(t, "clean divergence")
	inspector := GitInspector{Binary: "git", Env: env}
	candidateName := "sync/multi-source-repo-sync/globex-to-acme/develop/" + targetSHA[:12] + "-" + sourceSHA[:12]

	candidate, err := inspector.PushCandidate(context.Background(), tixRemote, globexRemote, "develop", candidateName, targetSHA, sourceSHA)
	if err != nil {
		t.Fatalf("PushCandidate() error = %v", err)
	}
	if candidate.AlreadyPushed || candidate.BranchName != candidateName {
		t.Fatalf("candidate = %+v, want newly pushed candidate %q", candidate, candidateName)
	}
	assertRemoteRef(t, tixRemote, "refs/heads/develop", targetSHA)
	assertRemoteRef(t, globexRemote, "refs/heads/develop", sourceSHA)
	assertRemoteRef(t, tixRemote, "refs/heads/"+candidateName, candidate.CommitSHA)

	verifyPath := filepath.Join(t.TempDir(), "verify")
	gitRun(t, "", "clone", "--quiet", tixRemote, verifyPath)
	gitRun(t, verifyPath, "fetch", "--quiet", tixRemote, "refs/heads/"+candidateName)
	baseInCandidate := gitRun(t, verifyPath, "merge-base", targetSHA, candidate.CommitSHA)
	if strings.TrimSpace(string(baseInCandidate)) != targetSHA {
		t.Fatalf("candidate's merge base = %q, want target head %q", strings.TrimSpace(string(baseInCandidate)), targetSHA)
	}
	if _, err := gitRunOptional(verifyPath, "merge-base", "--is-ancestor", sourceSHA, candidate.CommitSHA); err != nil {
		t.Fatalf("source head is not preserved in candidate history: %v", err)
	}

	repeated, err := inspector.PushCandidate(context.Background(), tixRemote, globexRemote, "develop", candidateName, targetSHA, sourceSHA)
	if err != nil {
		t.Fatalf("idempotent PushCandidate() error = %v", err)
	}
	if !repeated.AlreadyPushed || repeated.CommitSHA != candidate.CommitSHA {
		t.Fatalf("repeated candidate = %+v, want the same existing candidate", repeated)
	}
}

func TestPushCandidateStopsOnConflictAndPreservesRemoteRefs(t *testing.T) {
	t.Parallel()

	tixRemote, globexRemote, targetSHA, sourceSHA, env := pushHistoryPair(t, "conflicting divergence")
	candidateName := "sync/multi-source-repo-sync/globex-to-acme/develop/test-conflict"
	_, err := (GitInspector{Binary: "git", Env: env}).PushCandidate(
		context.Background(), tixRemote, globexRemote, "develop", candidateName, targetSHA, sourceSHA,
	)
	if err == nil || !strings.Contains(err.Error(), "merge conflicts") {
		t.Fatalf("PushCandidate() error = %v, want conflict stop", err)
	}
	assertRemoteRef(t, tixRemote, "refs/heads/develop", targetSHA)
	assertRemoteRef(t, globexRemote, "refs/heads/develop", sourceSHA)
	assertRemoteAbsent(t, tixRemote, "refs/heads/"+candidateName)
}

func TestPushCandidateStopsOnUnrelatedHistories(t *testing.T) {
	t.Parallel()

	tixRemote, globexRemote, targetSHA, sourceSHA, env := pushHistoryPair(t, "unrelated histories")
	candidateName := "sync/multi-source-repo-sync/globex-to-acme/develop/unrelated"
	_, err := (GitInspector{Binary: "git", Env: env}).PushCandidate(
		context.Background(), tixRemote, globexRemote, "develop", candidateName, targetSHA, sourceSHA,
	)
	if err == nil || !strings.Contains(err.Error(), "no common ancestor") {
		t.Fatalf("PushCandidate() error = %v, want unrelated-history stop", err)
	}
	assertRemoteRef(t, tixRemote, "refs/heads/develop", targetSHA)
	assertRemoteRef(t, globexRemote, "refs/heads/develop", sourceSHA)
	assertRemoteAbsent(t, tixRemote, "refs/heads/"+candidateName)
}

func TestPushCandidateNeverOverwritesExistingCandidateBranch(t *testing.T) {
	t.Parallel()

	tixRemote, globexRemote, targetSHA, sourceSHA, env := pushHistoryPair(t, "Acme behind")
	inspector := GitInspector{Binary: "git", Env: env}
	candidateName := "sync/multi-source-repo-sync/globex-to-acme/develop/collision"
	candidate, err := inspector.PushCandidate(context.Background(), tixRemote, globexRemote, "develop", candidateName, targetSHA, sourceSHA)
	if err != nil {
		t.Fatalf("initial PushCandidate() error = %v", err)
	}
	gitRun(t, tixRemote, "update-ref", "refs/heads/"+candidateName, targetSHA)

	_, err = inspector.PushCandidate(context.Background(), tixRemote, globexRemote, "develop", candidateName, targetSHA, sourceSHA)
	if err == nil || !strings.Contains(err.Error(), "already exists with different history") {
		t.Fatalf("second PushCandidate() error = %v, want collision stop", err)
	}
	assertRemoteRef(t, tixRemote, "refs/heads/"+candidateName, targetSHA)
	if candidate.CommitSHA == targetSHA {
		t.Fatal("test candidate should not equal target branch head")
	}
}

func TestPushCandidateRejectsStaleHeadsWithoutPushing(t *testing.T) {
	t.Parallel()

	tixRemote, globexRemote, targetSHA, sourceSHA, env := pushHistoryPair(t, "Acme behind")
	candidateName := "sync/multi-source-repo-sync/globex-to-acme/develop/stale"
	_, err := (GitInspector{Binary: "git", Env: env}).PushCandidate(
		context.Background(), tixRemote, globexRemote, "develop", candidateName, strings.Repeat("0", 40), sourceSHA,
	)
	if err == nil || !strings.Contains(err.Error(), "changed since planning") {
		t.Fatalf("PushCandidate() error = %v, want stale-head stop", err)
	}
	assertRemoteRef(t, tixRemote, "refs/heads/develop", targetSHA)
	assertRemoteAbsent(t, tixRemote, "refs/heads/"+candidateName)
}

func TestPushCandidateRejectsDevTestBeforeUsingGit(t *testing.T) {
	t.Parallel()

	_, err := New().PushCandidate(context.Background(), "unused", "unused", "dev-test", "sync/dev-test", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err == nil || !strings.Contains(err.Error(), "preserve-only") {
		t.Fatalf("PushCandidate() error = %v, want dev-test preserve-only rejection", err)
	}
}

func historyPair(t *testing.T, scenario string) (string, string, string, string) {
	t.Helper()

	base := t.TempDir()
	seed := filepath.Join(base, "seed")
	createRepository(t, seed, "base.txt", "base\n")
	seedSHA := gitOutput(t, seed, "rev-parse", "HEAD")
	tixNow := cloneRepository(t, seed, filepath.Join(base, "acme"))
	globex := cloneRepository(t, seed, filepath.Join(base, "globex"))

	switch scenario {
	case "same":
	case "Acme behind":
		commitFile(t, globex, "globex.txt", "globex\n")
	case "Globex behind":
		commitFile(t, tixNow, "acme.txt", "acme\n")
	case "clean divergence":
		commitFile(t, tixNow, "acme.txt", "acme\n")
		commitFile(t, globex, "globex.txt", "globex\n")
	case "conflicting divergence":
		commitFile(t, tixNow, "base.txt", "Acme change\n")
		commitFile(t, globex, "base.txt", "Globex change\n")
	case "unrelated histories":
		globex = filepath.Join(base, "unrelated")
		createRepository(t, globex, "base.txt", "separate history\n")
	default:
		t.Fatalf("unknown test history scenario %q", scenario)
	}

	leftSHA := gitOutput(t, tixNow, "rev-parse", "HEAD")
	rightSHA := gitOutput(t, globex, "rev-parse", "HEAD")
	if scenario == "same" && leftSHA != seedSHA {
		t.Fatalf("clone head = %s, want seed head %s", leftSHA, seedSHA)
	}
	return tixNow, globex, leftSHA, rightSHA
}

func pushHistoryPair(t *testing.T, scenario string) (string, string, string, string, []string) {
	t.Helper()

	root := t.TempDir()
	tixSeed := filepath.Join(root, "acme-seed")
	createRepository(t, tixSeed, "base.txt", "base\n")
	globexSeed := tixSeed
	if scenario == "unrelated histories" {
		globexSeed = filepath.Join(root, "globex-seed")
		createRepository(t, globexSeed, "base.txt", "separate history\n")
	}
	tixWorking := cloneRepository(t, tixSeed, filepath.Join(root, "acme-working"))
	globexWorking := cloneRepository(t, globexSeed, filepath.Join(root, "globex-working"))
	tixRemote := filepath.Join(root, "acme.git")
	globexRemote := filepath.Join(root, "globex.git")
	gitRun(t, "", "clone", "--quiet", "--bare", tixSeed, tixRemote)
	gitRun(t, "", "clone", "--quiet", "--bare", globexSeed, globexRemote)

	switch scenario {
	case "Acme behind":
		commitFile(t, globexWorking, "globex.txt", "globex\n")
		gitRun(t, globexWorking, "push", "--quiet", globexRemote, "develop")
	case "clean divergence":
		commitFile(t, tixWorking, "acme.txt", "acme\n")
		gitRun(t, tixWorking, "push", "--quiet", tixRemote, "develop")
		commitFile(t, globexWorking, "globex.txt", "globex\n")
		gitRun(t, globexWorking, "push", "--quiet", globexRemote, "develop")
	case "conflicting divergence":
		commitFile(t, tixWorking, "base.txt", "Acme change\n")
		gitRun(t, tixWorking, "push", "--quiet", tixRemote, "develop")
		commitFile(t, globexWorking, "base.txt", "Globex change\n")
		gitRun(t, globexWorking, "push", "--quiet", globexRemote, "develop")
	case "unrelated histories":
	default:
		t.Fatalf("unknown push history scenario %q", scenario)
	}

	targetSHA := gitOutput(t, tixRemote, "rev-parse", "refs/heads/develop")
	sourceSHA := gitOutput(t, globexRemote, "rev-parse", "refs/heads/develop")
	globalConfig := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[user]\n\tname = Sync Test\n\temail = sync-test@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return tixRemote, globexRemote, targetSHA, sourceSHA, []string{"GIT_CONFIG_GLOBAL=" + globalConfig}
}

func assertRemoteRef(t *testing.T, remote string, ref string, wantSHA string) {
	t.Helper()

	output := gitRun(t, "", "ls-remote", "--heads", "--refs", remote, ref)
	fields := strings.Fields(string(output))
	if len(fields) != 2 || fields[0] != wantSHA || fields[1] != ref {
		t.Fatalf("remote ref output = %q, want %s %s", strings.TrimSpace(string(output)), wantSHA, ref)
	}
}

func assertRemoteAbsent(t *testing.T, remote string, ref string) {
	t.Helper()

	output := gitRun(t, "", "ls-remote", "--heads", "--refs", remote, ref)
	if len(strings.TrimSpace(string(output))) != 0 {
		t.Fatalf("remote ref %s unexpectedly exists: %q", ref, strings.TrimSpace(string(output)))
	}
}

func createRepository(t *testing.T, path string, fileName string, content string) {
	t.Helper()

	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, "", "init", "--quiet", "--initial-branch=develop", path)
	configureGitIdentity(t, path)
	commitFile(t, path, fileName, content)
}

func cloneRepository(t *testing.T, source string, destination string) string {
	t.Helper()

	gitRun(t, "", "clone", "--quiet", source, destination)
	configureGitIdentity(t, destination)
	return destination
}

func configureGitIdentity(t *testing.T, repository string) {
	t.Helper()

	gitRun(t, repository, "config", "user.name", "Sync Test")
	gitRun(t, repository, "config", "user.email", "sync-test@example.invalid")
}

func commitFile(t *testing.T, repository string, name string, content string) {
	t.Helper()

	path := filepath.Join(repository, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", "--", name)
	gitRun(t, repository, "commit", "--quiet", "-m", "update "+name)
}

func gitOutput(t *testing.T, repository string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(gitRun(t, repository, args...)))
}

func gitRun(t *testing.T, repository string, args ...string) []byte {
	t.Helper()

	commandArgs := append([]string(nil), args...)
	if repository != "" {
		commandArgs = append([]string{"-C", repository}, commandArgs...)
	}
	command := exec.Command("git", commandArgs...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(commandArgs, " "), err, output)
	}
	return output
}

func gitRunOptional(repository string, args ...string) ([]byte, error) {
	commandArgs := append([]string{"-C", repository}, args...)
	command := exec.Command("git", commandArgs...)
	output, err := command.CombinedOutput()
	return output, err
}

// propagationRepos: a source with a feature branch ahead of develop, and targets.
func propagationRepos(t *testing.T) (source string, target string, featureSHA string) {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	createRepository(t, seed, "base.txt", "base\n")
	work := cloneRepository(t, seed, filepath.Join(root, "work"))
	gitRun(t, work, "checkout", "--quiet", "-b", "feat/edge")
	commitFile(t, work, "feature.txt", "feature\n")
	featureSHA = gitOutput(t, work, "rev-parse", "HEAD")
	source = filepath.Join(root, "source.git")
	target = filepath.Join(root, "target.git")
	gitRun(t, "", "clone", "--quiet", "--bare", seed, source)
	gitRun(t, "", "clone", "--quiet", "--bare", seed, target)
	gitRun(t, work, "push", "--quiet", source, "feat/edge")
	return source, target, featureSHA
}

func TestPushBranchCreatesTheBranchInTheTargetAndLeavesTheBaseAlone(t *testing.T) {
	t.Parallel()
	source, target, sha := propagationRepos(t)
	baseBefore := gitOutput(t, target, "rev-parse", "refs/heads/develop")

	got, err := New().PushBranch(context.Background(), target, source, "feat/edge", sha, "develop")
	if err != nil {
		t.Fatalf("PushBranch() error = %v", err)
	}
	if got.AlreadyPushed || got.Ahead != 1 || got.SHA != sha {
		t.Fatalf("result = %+v, want a new branch one commit ahead", got)
	}
	assertRemoteRef(t, target, "refs/heads/feat/edge", sha)
	assertRemoteRef(t, target, "refs/heads/develop", baseBefore)

	again, err := New().PushBranch(context.Background(), target, source, "feat/edge", sha, "develop")
	if err != nil || !again.AlreadyPushed {
		t.Fatalf("second push = %+v, %v; want it recognized as already there", again, err)
	}
}

func TestPushBranchNeverOverwritesABranchWithOtherHistory(t *testing.T) {
	t.Parallel()
	source, target, sha := propagationRepos(t)
	other := filepath.Join(t.TempDir(), "other")
	cloned := cloneRepository(t, target, other)
	gitRun(t, cloned, "checkout", "--quiet", "-b", "feat/edge")
	commitFile(t, cloned, "different.txt", "different\n")
	gitRun(t, cloned, "push", "--quiet", target, "feat/edge")
	existing := gitOutput(t, target, "rev-parse", "refs/heads/feat/edge")

	_, err := New().PushBranch(context.Background(), target, source, "feat/edge", sha, "develop")
	if err == nil || !strings.Contains(err.Error(), "different history") {
		t.Fatalf("error = %v, want a refusal to touch the existing branch", err)
	}
	assertRemoteRef(t, target, "refs/heads/feat/edge", existing)
}

func TestPushBranchStopsWhenThereIsNothingToProposeOrNoSharedHistory(t *testing.T) {
	t.Parallel()
	source, target, sha := propagationRepos(t)
	// The target base already has the feature commit.
	gitRun(t, "", "-C", target, "fetch", "--quiet", source, "feat/edge:refs/heads/develop")
	if _, err := New().PushBranch(context.Background(), target, source, "feat/edge", sha, "develop"); err == nil || !strings.Contains(err.Error(), "nothing to propose") {
		t.Fatalf("error = %v, want nothing to propose", err)
	}
	assertRemoteAbsent(t, target, "refs/heads/feat/edge")

	unrelated := filepath.Join(t.TempDir(), "unrelated")
	createRepository(t, unrelated, "base.txt", "separate history\n")
	if _, err := New().PushBranch(context.Background(), unrelated, source, "feat/edge", sha, "develop"); err == nil || !strings.Contains(err.Error(), "no common ancestor") {
		t.Fatalf("error = %v, want no common ancestor", err)
	}
}

func TestPushBranchRefusesStaleHeadsAndForbiddenNames(t *testing.T) {
	t.Parallel()
	source, target, sha := propagationRepos(t)
	stale := strings.Repeat("0", 40)
	if _, err := New().PushBranch(context.Background(), target, source, "feat/edge", stale, "develop"); err == nil || !strings.Contains(err.Error(), "changed since planning") {
		t.Fatalf("error = %v, want a stale-head refusal", err)
	}
	for _, name := range []string{"dev-test", "develop", "", "bad name", "a..b"} {
		if _, err := New().PushBranch(context.Background(), target, source, name, sha, "develop"); err == nil {
			t.Errorf("branch %q must be refused", name)
		}
	}
	assertRemoteAbsent(t, target, "refs/heads/feat/edge")
}
