package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
)

func TestResolveRejectsUnsafeRequestsBeforeNetwork(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"prepare", "--service", "api-gateway", "--branch", "dev-test", "--target", "globex"},
		{"prepare", "--service", "unknown", "--branch", "main", "--target", "globex"},
		{"prepare", "--service", "api-gateway", "--branch", "main"},
		{"prepare", "--service", "api-gateway", "--branch", "main", "--target", "globex", "--workspace", "/tmp/forbidden-resolution"},
		{"check", "--workspace", "unused", "--", "echo", "must-not-run"},
		{"check", "--trusted-local"},
		{"status"},
		{"publish", "--workspace", "missing"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		withConfig := []string{"resolve", args[0], "--config", "../../config/repos.example.json"}
		withConfig = append(withConfig, args[1:]...)
		if code := Run(context.Background(), withConfig, strings.NewReader(""), &stdout, &stderr); code == 0 {
			t.Errorf("expected refusal for %v: %s", args, &stdout)
		}
	}
}

func cliGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func cliWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cliGit(t, dir, "init", "--quiet", "--template=")
	cliGit(t, dir, "config", "user.name", "Resolution Test")
	cliGit(t, dir, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("reviewed code\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cliGit(t, dir, "add", "base.txt")
	cliGit(t, dir, "commit", "--quiet", "-m", "base")
	sha := cliGit(t, dir, "rev-parse", "HEAD")
	state := gitinspect.ResolutionState{Version: 1, Service: "api-gateway", Branch: "main", Target: "globex", Source: "acme", TargetSHA: sha, SourceSHA: sha}
	if err := os.WriteFile(filepath.Join(dir, ".git", "MERGE_HEAD"), []byte(sha+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gitinspect.SaveResolution(dir, state); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Invoked only by the test executable as an explicitly supplied local command.
func TestResolutionCheckHelper(t *testing.T) {
	var action string
	for index, arg := range os.Args {
		if arg == "--" && index+1 < len(os.Args) {
			action = os.Args[index+1]
		}
	}
	if action == "" {
		return
	}
	switch action {
	case "success":
		fmt.Println("explicit test passed")
	case "fail":
		os.Exit(1)
	case "mutate":
		if err := os.WriteFile("base.txt", []byte("test changed tracked code\n"), 0o600); err != nil {
			os.Exit(2)
		}
	default:
		os.Exit(3)
	}
}

func TestResolveCheckRecordsExactTreeAndRevokesFailedRetry(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("local execution currently Linux only")
	}
	t.Parallel()
	dir := cliWorkspace(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"success", "fail", "success", "mutate"} {
		var stdout, stderr bytes.Buffer
		args := []string{"resolve", "check", "--config", "../../config/repos.example.json", "--workspace", dir, "--trusted-local", "--", executable, "-test.run=^TestResolutionCheckHelper$", "--", action}
		code := Run(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
		if (code == 0) != (action == "success") {
			t.Fatalf("action %s code=%d: %s %s", action, code, &stdout, &stderr)
		}
		state, err := gitinspect.LoadResolution(dir)
		if err != nil {
			t.Fatal(err)
		}
		if (state.Tree != "") != (action == "success") {
			t.Fatalf("action %s recorded invalid success: %+v", action, state)
		}
	}
}

type noRemoteWrites struct {
	calls int
}

func (c *noRemoteWrites) MergeCommitAllowed(_ context.Context, _ string, _ string) (bool, error) {
	c.calls++
	return true, nil
}

func (c *noRemoteWrites) EnsurePullRequest(_ context.Context, _ string, _ string, _ string, _ string, _ string, _ string) (github.PullRequestResult, error) {
	c.calls++
	return github.PullRequestResult{}, nil
}

func TestResolutionPublicationRequiresReviewAndExactConfirmation(t *testing.T) {
	t.Parallel()
	dir := cliWorkspace(t)
	i := gitinspect.New()
	tree, err := i.ResolutionTree(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.RecordCheck(context.Background(), dir, tree, []string{"fixture-test"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("../../config/repos.example.json")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		apply    bool
		reviewed bool
		phrase   string
		wantCode int
	}{
		{wantCode: 0},
		{apply: true, wantCode: 2},
		{apply: true, reviewed: true, phrase: "yes\n", wantCode: 0},
		{apply: true, reviewed: true, phrase: "publish api-gateway main globex-to-acme\n", wantCode: 0},
		{apply: true, reviewed: true, phrase: "publish api-gateway main acme-to-globex", wantCode: 0},
	}
	for _, tc := range cases {
		client := &noRemoteWrites{}
		var stdout, stderr bytes.Buffer
		code := publishResolution(context.Background(), cfg, i, client, dir, tc.apply, tc.reviewed, strings.NewReader(tc.phrase), &stdout, &stderr)
		if code != tc.wantCode || client.calls != 0 {
			t.Fatalf("unexpected publication: code=%d calls=%d stderr=%s", code, client.calls, &stderr)
		}
	}
}

func TestResolveStatusIsResumableAndLocal(t *testing.T) {
	t.Parallel()
	dir := cliWorkspace(t)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"resolve", "status", "--config", "../../config/repos.example.json", "--workspace", dir}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "api-gateway / main") {
		t.Fatalf("status code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
}

func TestResolveRejectsSymlinkIntoTemporaryStorage(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	link := filepath.Join(base, "link")
	if err := os.Symlink(os.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspacePath(filepath.Join(link, "not-created")); err == nil {
		t.Fatal("symlink into temporary storage accepted")
	}
}

type resolutionPRFixture struct {
	allowed bool
	fail    bool
	prs     int
	body    string
}

func (c *resolutionPRFixture) MergeCommitAllowed(_ context.Context, _ string, _ string) (bool, error) {
	return c.allowed, nil
}

func (c *resolutionPRFixture) EnsurePullRequest(_ context.Context, _ string, _ string, _ string, _ string, _ string, body string) (github.PullRequestResult, error) {
	c.prs++
	c.body = body
	if c.fail {
		return github.PullRequestResult{}, errors.New("synthetic GitHub outage")
	}
	return github.PullRequestResult{Number: 1, URL: "https://example.invalid/pr/1", Created: c.prs == 1}, nil
}

func TestResolutionPublishFullJourneyAndPRFailureRecovery(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	seed := cliWorkspace(t)
	cliGit(t, seed, "branch", "-M", "main")
	sourceWork := filepath.Join(root, "source-work")
	cliGit(t, root, "clone", "--quiet", seed, sourceWork)
	cliGit(t, sourceWork, "config", "user.name", "Resolution Test")
	cliGit(t, sourceWork, "config", "user.email", "test@example.invalid")
	for dir, text := range map[string]string{seed: "target change\n", sourceWork: "source change\n"} {
		if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		cliGit(t, dir, "add", "base.txt")
		cliGit(t, dir, "commit", "--quiet", "-m", "diverge")
	}
	targetSHA := cliGit(t, seed, "rev-parse", "HEAD")
	sourceSHA := cliGit(t, sourceWork, "rev-parse", "HEAD")
	targetRemote, sourceRemote := filepath.Join(root, "target.git"), filepath.Join(root, "source.git")
	cliGit(t, root, "clone", "--quiet", "--bare", seed, targetRemote)
	cliGit(t, root, "clone", "--quiet", "--bare", sourceWork, sourceRemote)
	global := filepath.Join(root, "gitconfig")
	configuration := fmt.Sprintf("[user]\n name = Resolution Test\n email = test@example.invalid\n[url %q]\n insteadOf = https://github.com/Globex-Org/api-gateway.git\n[url %q]\n insteadOf = https://github.com/Acme-Org/api-gateway.git\n", targetRemote, sourceRemote)
	if err := os.WriteFile(global, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	// Only local file transport is allowed: a mapping error cannot reach GitHub.
	i := gitinspect.GitInspector{Env: []string{"GIT_CONFIG_GLOBAL=" + global, "GIT_ALLOW_PROTOCOL=file"}}
	dir := filepath.Join(root, "resolution")
	_, err := i.PrepareResolution(context.Background(), dir, gitinspect.ResolutionState{Service: "api-gateway", Branch: "main", Target: "globex", Source: "acme", TargetSHA: targetSHA, SourceSHA: sourceSHA}, targetRemote, sourceRemote)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("reviewed integration\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cliGit(t, dir, "add", "base.txt")
	tree, err := i.ResolutionTree(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.RecordCheck(context.Background(), dir, tree, []string{"fixture-test", "--private-value=must-not-persist"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("../../config/repos.example.json")
	if err != nil {
		t.Fatal(err)
	}
	client := &resolutionPRFixture{}
	var stdout, stderr bytes.Buffer
	invoke := func() int {
		return publishResolution(context.Background(), cfg, i, client, dir, true, true, strings.NewReader("publish api-gateway main acme-to-globex\n"), &stdout, &stderr)
	}
	if code := invoke(); code != 1 || client.prs != 0 {
		t.Fatal("disabled merge commits must block before pushing")
	}
	if got := cliGit(t, targetRemote, "for-each-ref", "--format=%(refname)", "refs/heads/sync/"); got != "" {
		t.Fatalf("unexpected candidate before permission: %s", got)
	}
	client.allowed, client.fail = true, true
	if code := invoke(); code != 1 || client.prs != 1 {
		t.Fatalf("expected partial publication: code=%d prs=%d %s", code, client.prs, &stderr)
	}
	candidate := cliGit(t, targetRemote, "for-each-ref", "--format=%(objectname)", "refs/heads/sync/")
	if candidate == "" {
		t.Fatal("PR failure must preserve the pushed candidate")
	}
	client.fail = false
	if code := invoke(); code != 0 || client.prs != 2 {
		t.Fatalf("retry failed: code=%d prs=%d %s", code, client.prs, &stderr)
	}
	if got := cliGit(t, targetRemote, "for-each-ref", "--format=%(objectname)", "refs/heads/sync/"); got != candidate {
		t.Fatal("retry must reuse the exact candidate")
	}
	if strings.Contains(client.body, "must-not-persist") || !strings.Contains(client.body, "argument digest") {
		t.Fatal("raw test arguments must not leak to PR bodies")
	}
	if got := cliGit(t, targetRemote, "rev-parse", "refs/heads/main"); got != targetSHA {
		t.Fatal("target base moved")
	}
	if got := cliGit(t, sourceRemote, "rev-parse", "refs/heads/main"); got != sourceSHA {
		t.Fatal("source base moved")
	}
}
