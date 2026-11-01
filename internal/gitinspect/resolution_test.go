package gitinspect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func prepareTestResolution(t *testing.T, scenario string) (GitInspector, string, string, string, ResolutionStatus) {
	t.Helper()
	target, source, targetSHA, sourceSHA, env := pushHistoryPair(t, scenario)
	i := GitInspector{Binary: "git", Env: env}
	directory := filepath.Join(t.TempDir(), "resolution")
	state := ResolutionState{Service: "service", Branch: "develop", Target: "acme", Source: "globex", TargetSHA: targetSHA, SourceSHA: sourceSHA}
	status, err := i.PrepareResolution(context.Background(), directory, state, target, source)
	if err != nil {
		t.Fatal(err)
	}
	return i, directory, target, source, status
}

func stageResolution(t *testing.T, directory string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, "base.txt"), []byte("reviewed Acme and Globex behavior\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, directory, "add", "--", "base.txt")
}

func recordTestCheck(t *testing.T, i GitInspector, directory string) string {
	t.Helper()
	tree, err := i.ResolutionTree(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.RecordCheck(context.Background(), directory, tree, []string{"explicit-test-command"}); err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestResolutionPreservesConflictStagesAndNeverTouchesBases(t *testing.T) {
	t.Parallel()
	i, directory, target, source, status := prepareTestResolution(t, "conflicting divergence")
	if len(status.Conflicts) != 1 || status.Conflicts[0] != "base.txt" {
		t.Fatalf("conflicts = %#v", status.Conflicts)
	}
	for _, stage := range []string{":1:base.txt", ":2:base.txt", ":3:base.txt"} {
		if gitOutput(t, directory, "show", stage) == "" {
			t.Fatalf("missing base/target/source stage %s", stage)
		}
	}
	if _, err := i.ResolutionTree(context.Background(), directory); err == nil {
		t.Fatal("unresolved merge must block checking")
	}
	if _, err := i.PushResolution(context.Background(), directory, target, source); err == nil {
		t.Fatal("unresolved merge must block pushing")
	}
	assertRemoteRef(t, target, "refs/heads/develop", status.State.TargetSHA)
	assertRemoteRef(t, source, "refs/heads/develop", status.State.SourceSHA)
	stageResolution(t, directory)
	if _, err := i.CheckedResolution(context.Background(), directory); err == nil {
		t.Fatal("reviewed but untested tree must block publication")
	}
	tree := recordTestCheck(t, i, directory)
	candidate, err := i.PushResolution(context.Background(), directory, target, source)
	if err != nil {
		t.Fatal(err)
	}
	parents := gitOutput(t, directory, "show", "-s", "--format=%P %T", candidate.CommitSHA)
	if parents != status.State.TargetSHA+" "+status.State.SourceSHA+" "+tree {
		t.Fatalf("commit does not preserve exact original parents/tree: %s", parents)
	}
	assertRemoteRef(t, target, "refs/heads/"+candidate.BranchName, candidate.CommitSHA)
	assertRemoteRef(t, target, "refs/heads/develop", status.State.TargetSHA)
	assertRemoteRef(t, source, "refs/heads/develop", status.State.SourceSHA)
	repeated, err := i.PushResolution(context.Background(), directory, target, source)
	if err != nil || !repeated.AlreadyPushed || repeated.CommitSHA != candidate.CommitSHA {
		t.Fatalf("retry should reuse exact candidate: %+v, %v", repeated, err)
	}
	gitRun(t, target, "update-ref", "refs/heads/"+candidate.BranchName, status.State.TargetSHA)
	if _, err := i.PushResolution(context.Background(), directory, target, source); err == nil {
		t.Fatal("candidate collision must never be overwritten")
	}
	assertRemoteRef(t, target, "refs/heads/"+candidate.BranchName, status.State.TargetSHA)
}

func TestResolutionAutomaticallyCombinesOnlyCleanChanges(t *testing.T) {
	t.Parallel()
	i, directory, _, _, status := prepareTestResolution(t, "clean divergence")
	if len(status.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", status.Conflicts)
	}
	for _, file := range []string{"acme.txt", "globex.txt"} {
		if _, err := os.Stat(filepath.Join(directory, file)); err != nil {
			t.Fatalf("clean change lost: %s: %v", file, err)
		}
	}
	if _, err := i.ResolutionTree(context.Background(), directory); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(directory, "base.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.PrepareResolution(context.Background(), directory, status.State, "unused", "unused"); err == nil {
		t.Fatal("must not reuse an existing workspace")
	}
	after, _ := os.ReadFile(filepath.Join(directory, "base.txt"))
	if string(after) != string(before) {
		t.Fatal("existing workspace was changed")
	}
}

func TestResolutionRejectsAbortedMerge(t *testing.T) {
	t.Parallel()
	i, directory, _, _, _ := prepareTestResolution(t, "clean divergence")
	gitRun(t, directory, "merge", "--abort")
	if _, err := i.ResolutionTree(context.Background(), directory); err == nil || !strings.Contains(err.Error(), "aborted or changed") {
		t.Fatalf("aborted integration should not become a source-dropping resolution: %v", err)
	}
}

func TestResolutionRejectsMarkersAndInvalidatesChangedTree(t *testing.T) {
	t.Parallel()
	i, directory, _, _, _ := prepareTestResolution(t, "conflicting divergence")
	gitRun(t, directory, "add", "--", "base.txt")
	if _, err := i.ResolutionTree(context.Background(), directory); err == nil {
		t.Fatal("staging conflict markers must not bypass review")
	}
	stageResolution(t, directory)
	tree := recordTestCheck(t, i, directory)
	if err := os.WriteFile(filepath.Join(directory, "base.txt"), []byte("modified after test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := i.CheckedResolution(context.Background(), directory); err == nil {
		t.Fatal("unstaged change must block publishing")
	}
	gitRun(t, directory, "add", "--", "base.txt")
	if _, err := i.CheckedResolution(context.Background(), directory); err == nil {
		t.Fatal("new staged tree must invalidate previous successful check")
	}
	if err := i.RecordCheck(context.Background(), directory, tree, []string{"test"}); err == nil {
		t.Fatal("test that changes the tree cannot record success")
	}
	recordTestCheck(t, i, directory)
	if err := os.WriteFile(filepath.Join(directory, "forgotten.txt"), []byte("new work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := i.CheckedResolution(context.Background(), directory); err == nil {
		t.Fatal("untracked work must not be silently omitted")
	}
}

func TestResolutionBlocksStaleRemoteAndTamperedCandidate(t *testing.T) {
	t.Parallel()
	for _, side := range []string{"target", "source"} {
		t.Run(side, func(t *testing.T) {
			t.Parallel()
			i, directory, target, source, status := prepareTestResolution(t, "conflicting divergence")
			stageResolution(t, directory)
			recordTestCheck(t, i, directory)
			remote := target
			sha := status.State.TargetSHA
			if side == "source" {
				remote = source
				sha = status.State.SourceSHA
			}
			gitRun(t, remote, "update-ref", "refs/heads/develop", sha+"^")
			if _, err := i.PushResolution(context.Background(), directory, target, source); err == nil || !strings.Contains(err.Error(), "moved") {
				t.Fatalf("stale %s head should block push: %v", side, err)
			}
			branches := gitOutput(t, target, "for-each-ref", "--format=%(refname)", "refs/heads/sync/")
			if branches != "" {
				t.Fatalf("unexpected remote candidate: %s", branches)
			}
		})
	}
	i, directory, target, source, status := prepareTestResolution(t, "clean divergence")
	recordTestCheck(t, i, directory)
	state, err := LoadResolution(directory)
	if err != nil {
		t.Fatal(err)
	}
	state.Candidate = status.State.TargetSHA
	if err := SaveResolution(directory, state); err != nil {
		t.Fatal(err)
	}
	if _, err := i.PushResolution(context.Background(), directory, target, source); err == nil {
		t.Fatal("tampered saved commit must not bypass tree/parent verification")
	}
}

func TestResolutionRejectsUnrelatedOrStalePreparation(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"unrelated histories", "conflicting divergence"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			target, source, targetSHA, sourceSHA, env := pushHistoryPair(t, scenario)
			if scenario == "conflicting divergence" {
				sourceSHA = strings.Repeat("0", 40)
			}
			i := GitInspector{Env: env}
			_, err := i.PrepareResolution(context.Background(), filepath.Join(t.TempDir(), "workspace"), ResolutionState{
				Service: "service", Branch: "develop", Target: "globex", Source: "acme", TargetSHA: targetSHA, SourceSHA: sourceSHA,
			}, target, source)
			if err == nil {
				t.Fatal("unsafe preparation should be blocked")
			}
		})
	}
}

func TestResolutionRejectsUnionDriversAndCheckoutFilters(t *testing.T) {
	t.Parallel()
	for _, attributes := range []string{"base.txt merge=union\n", "base.txt filter=custom\n"} {
		t.Run(strings.TrimSpace(attributes), func(t *testing.T) {
			t.Parallel()
			target, source, targetSHA, _, env := pushHistoryPair(t, "conflicting divergence")
			work := cloneRepository(t, source, filepath.Join(t.TempDir(), "source-work"))
			commitFile(t, work, ".gitattributes", attributes)
			gitRun(t, work, "push", "--quiet", source, "develop")
			sourceSHA := gitOutput(t, work, "rev-parse", "HEAD")
			i := GitInspector{Env: env}
			_, err := i.PrepareResolution(context.Background(), filepath.Join(t.TempDir(), "resolution"), ResolutionState{Service: "service", Branch: "develop", Target: "acme", Source: "globex", TargetSHA: targetSHA, SourceSHA: sourceSHA}, target, source)
			if err == nil || !strings.Contains(err.Error(), "driver or checkout filter") {
				t.Fatalf("unsafe attributes accepted: %s %v", attributes, err)
			}
		})
	}
}

func TestResolutionListsBinaryAndModifyDeleteDecisions(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"binary", "modify-delete"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			target, source, _, _, env := pushHistoryPair(t, "clean divergence")
			left := cloneRepository(t, target, filepath.Join(t.TempDir(), "left"))
			right := cloneRepository(t, source, filepath.Join(t.TempDir(), "right"))
			if kind == "binary" {
				commitFile(t, left, "base.txt", "target\x00bytes")
				commitFile(t, right, "base.txt", "source\x00bytes")
			} else {
				gitRun(t, left, "rm", "base.txt")
				gitRun(t, left, "commit", "--quiet", "-m", "delete base")
				commitFile(t, right, "base.txt", "source modified\n")
			}
			gitRun(t, left, "push", "--quiet", target, "develop")
			gitRun(t, right, "push", "--quiet", source, "develop")
			i := GitInspector{Env: env}
			directory := filepath.Join(t.TempDir(), "resolution")
			status, err := i.PrepareResolution(context.Background(), directory, ResolutionState{
				Service: "service", Branch: "develop", Target: "acme", Source: "globex",
				TargetSHA: gitOutput(t, left, "rev-parse", "HEAD"), SourceSHA: gitOutput(t, right, "rev-parse", "HEAD"),
			}, target, source)
			if err != nil || len(status.Conflicts) != 1 || status.Conflicts[0] != "base.txt" {
				t.Fatalf("expected explicit binary/delete decision: %+v %v", status, err)
			}
			if _, err := i.ResolutionTree(context.Background(), directory); err == nil {
				t.Fatal("non-text conflict must block checks")
			}
			for _, cleanFile := range []string{"acme.txt", "globex.txt"} {
				if _, err := os.Stat(filepath.Join(directory, cleanFile)); err != nil {
					t.Fatalf("lost clean changes while resolving %s: %s", kind, cleanFile)
				}
			}
		})
	}
}

func TestResolutionRejectsCandidateCreatedBetweenPrecheckAndPush(t *testing.T) {
	t.Parallel()
	i, directory, target, source, status := prepareTestResolution(t, "clean divergence")
	recordTestCheck(t, i, directory)
	candidate, err := i.PushResolution(context.Background(), directory, target, source)
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/heads/" + candidate.BranchName
	// Remove only this disposable fixture's candidate, then simulate another
	// publisher creating an ancestor under that name just before negotiation.
	gitRun(t, target, "update-ref", "-d", ref)
	wrapper := filepath.Join(t.TempDir(), "git-race")
	script := `#!/bin/sh
for arg do
  if [ "$arg" = push ]; then
    git -C "$RACE_TARGET" update-ref "$RACE_REF" "$RACE_ANCESTOR" || exit 1
  fi
done
exec git "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	i.Binary = wrapper
	i.Env = append(i.Env, "RACE_TARGET="+target, "RACE_REF="+ref, "RACE_ANCESTOR="+status.State.TargetSHA)
	if _, err := i.PushResolution(context.Background(), directory, target, source); err == nil {
		t.Fatal("must reject concurrent existing-ref update even when it would fast-forward")
	}
	assertRemoteRef(t, target, ref, status.State.TargetSHA)
}
