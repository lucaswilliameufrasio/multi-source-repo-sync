package gitinspect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolutionReviewDoesNotRunExternalDiffAndStagingRemainsExplicit(t *testing.T) {
	t.Parallel()
	i, directory, _, _, _ := prepareTestResolution(t, "conflicting divergence")
	marker := filepath.Join(t.TempDir(), "external-diff-ran")
	script := filepath.Join(t.TempDir(), "external-diff")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch \"$REVIEW_DIFF_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, directory, "config", "diff.external", script)
	i.Env = append(i.Env, "REVIEW_DIFF_MARKER="+marker)
	diff, err := i.ResolutionDiff(context.Background(), directory)
	if err != nil || !strings.Contains(diff, "base.txt") {
		t.Fatalf("review failed: %v %s", err, diff)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("external diff was executed")
	}
	if err := i.StageResolutionPath(context.Background(), directory, "../outside"); err == nil {
		t.Fatal("arbitrary path should not be staged")
	}
	if err := os.WriteFile(filepath.Join(directory, "base.txt"), []byte("reviewed merge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := i.StageResolutionPath(context.Background(), directory, "base.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := i.ResolutionTree(context.Background(), directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(directory, "base.txt")); err != nil {
		t.Fatal(err)
	}
	if err := i.StageResolutionPath(context.Background(), directory, "base.txt"); err != nil {
		t.Fatalf("explicit deletion could not be staged: %v", err)
	}
}

func TestResolutionStagingTreatsGitPathspecSyntaxAsLiteralFilename(t *testing.T) {
	t.Parallel()
	i, directory, _, _, _ := prepareTestResolution(t, "clean divergence")
	for _, path := range []string{":(glob)*", "unreviewed.txt"} {
		if err := os.WriteFile(filepath.Join(directory, path), []byte("new work\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := i.StageResolutionPath(context.Background(), directory, ":(glob)*"); err != nil {
		t.Fatal(err)
	}
	pending, err := i.ResolutionUnstaged(context.Background(), directory)
	if err != nil || len(pending) != 1 || pending[0] != "unreviewed.txt" {
		t.Fatalf("staging interpreted a filename as a wildcard: %v %v", pending, err)
	}
}
