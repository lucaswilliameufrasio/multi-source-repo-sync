package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
)

type guideRepositoryFixture struct {
	left  map[string]string
	right map[string]string
	err   error
	calls int
}

func (r *guideRepositoryFixture) ReadRepository(_ context.Context, _ string, _ string, side string) (github.Snapshot, error) {
	r.calls++
	if r.err != nil {
		return github.Snapshot{}, r.err
	}
	if side == "acme" {
		return github.Snapshot{Branches: r.left}, nil
	}
	return github.Snapshot{Branches: r.right}, nil
}

func testGuide(t *testing.T, input string) (*guide, *bytes.Buffer, *[][]string) {
	t.Helper()
	cfg, err := config.Load("../../config/repos.example.json")
	if err != nil {
		t.Fatal(err)
	}
	// One pair, so the tests walk the menu without choosing between organizations.
	cfg.Version = 1
	cfg.Organizations = config.Organizations{{Label: "acme", Owner: "Acme-Org"}, {Label: "globex", Owner: "Globex-Org"}}
	cfg.OrganizationServices = nil
	output := &bytes.Buffer{}
	var calls [][]string
	g := &guide{ctx: context.Background(), cfg: cfg, configPath: "../../config/repos.example.json", home: t.TempDir(), input: bufferedInput(strings.NewReader(input)), output: output, errors: output,
		reader: &guideRepositoryFixture{left: map[string]string{"main": "left", "develop": "same"}, right: map[string]string{"main": "right", "develop": "same"}}}
	g.invoke = func(_ context.Context, args []string, _ io.Reader, _ io.Writer, _ io.Writer) int {
		calls = append(calls, append([]string(nil), args...))
		return 0
	}
	return g, output, &calls
}

func TestGuideStartsWithoutDiscoveringUserValues(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"guide", "--config", "../../config/repos.example.json"}, {"resolve", "guide", "--config", "../../config/repos.example.json"}} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), args, strings.NewReader("0\n"), &stdout, &stderr)
		if code != 0 || !strings.Contains(stdout.String(), "guia de uso") || !strings.Contains(stdout.String(), "0)") {
			t.Fatalf("args=%v code=%d stdout=%s stderr=%s", args, code, &stdout, &stderr)
		}
	}
}

func TestGuideRootEntryHelper(t *testing.T) {
	if os.Args[len(os.Args)-1] != "--run-guide-root" {
		return
	}
	os.Exit(Run(context.Background(), nil, os.Stdin, os.Stdout, os.Stderr))
}

func TestGuideNoArgumentsOpensMenu(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestGuideRootEntryHelper$", "--", "--run-guide-root")
	command.Dir = "../.."
	command.Env = append(os.Environ(), configEnv+"=config/repos.example.json")
	command.Stdin = strings.NewReader("0\n")
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "O que você quer fazer?") {
		t.Fatalf("no-argument guide failed: %v %s", err, output)
	}
}

func TestGuideMenuRejectsInvalidInputAndCancelsSafely(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", "0\n", "\n", "999\nbanana\n0\n", "1"} {
		g, _, calls := testGuide(t, input)
		err := g.run("")
		if !errors.Is(err, errGuideCancelled) || len(*calls) != 0 || g.reader.(*guideRepositoryFixture).calls != 0 {
			t.Fatalf("input=%q err=%v calls=%v", input, err, *calls)
		}
	}
}

func TestGuideListsOnlyAvailableDifferingAllowedBranches(t *testing.T) {
	t.Parallel()
	g, output, _ := testGuide(t, "1\n1\n")
	g.reader = &guideRepositoryFixture{left: map[string]string{"main": "a", "develop": "same", "dev-test": "left"}, right: map[string]string{"main": "b", "develop": "same", "dev-test": "right"}}
	service, _, branch, err := g.selectBranch()
	if err != nil || service != g.cfg.Services[0] || branch != "main" {
		t.Fatalf("selected %s %s: %v", service, branch, err)
	}
	if strings.Contains(output.String(), "dev-test") || !strings.Contains(output.String(), "1) main") {
		t.Fatalf("invalid branch options: %s", output)
	}
	g, _, calls := testGuide(t, "1\n")
	g.reader = &guideRepositoryFixture{err: errors.New("synthetic API unavailable")}
	if _, _, _, err := g.selectBranch(); err == nil || len(*calls) != 0 {
		t.Fatal("failed branch inventory must not guess values or perform actions")
	}
}

func TestGuideSyncDefaultsToPreviewAndRetainsBufferedConfirmation(t *testing.T) {
	t.Parallel()
	g, _, calls := testGuide(t, "1\n1\n\n")
	if err := g.newFlow(false); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || contains((*calls)[0], "--apply") {
		t.Fatalf("unexpected writes: %v", *calls)
	}
	g, output, _ := testGuide(t, "1\n1\npublicar\nsync api-gateway main acme/globex\nNEXT\n")
	var apply bool
	g.invoke = func(_ context.Context, args []string, input io.Reader, _ io.Writer, stderr io.Writer) int {
		if contains(args, "--apply") {
			confirmed, err := confirmApply(input, stderr, "api-gateway", "main", "acme/globex")
			if err != nil || !confirmed {
				t.Fatalf("buffer lost confirmation: %v %t", err, confirmed)
			}
			apply = true
		}
		return 0
	}
	if err := g.newFlow(false); err != nil || !apply {
		t.Fatalf("confirmed flow failed: %v %s", err, output)
	}
	if next, err := g.line(""); err != nil || next != "NEXT" {
		t.Fatalf("nested confirmation consumed subsequent menu input: %q %v", next, err)
	}
}

func TestGuidePrepareExplainsDirectionAndKeepsWorkspace(t *testing.T) {
	t.Parallel()
	g, output, _ := testGuide(t, "1\n1\n2\n")
	var selected []string
	g.invoke = func(_ context.Context, args []string, _ io.Reader, _ io.Writer, _ io.Writer) int {
		selected = args
		return 1 // Stop before preparing a real workspace/network operation.
	}
	if err := g.newFlow(true); err == nil {
		t.Fatal("failed preparation must be reported")
	}
	if !contains(selected, "acme") || !contains(selected, "--workspace") || contains(selected, "--apply") {
		t.Fatalf("wrong preparation: %v", selected)
	}
	if !strings.Contains(output.String(), "integrar mudanças de globex em Acme-Org") {
		t.Fatalf("PR direction unclear: %s", output)
	}
}

func TestGuideDiscoversWorkspacesAndSkipsInvalidOrSymlinkedEntries(t *testing.T) {
	t.Parallel()
	g, _, _ := testGuide(t, "1\n")
	root := filepath.Join(g.home, "git-worktrees", "api-gateway")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	original := cliWorkspace(t)
	directory := filepath.Join(root, "resolve-main-globex-fixture")
	if err := os.Rename(original, directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(directory, filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "unrelated-user-checkout"), 0o700); err != nil {
		t.Fatal(err)
	}
	choices, err := discoverWorkspaces(g.home, g.cfg)
	if err != nil || len(choices) != 1 || choices[0].directory != directory {
		t.Fatalf("discovery=%v err=%v", choices, err)
	}
	selected, err := g.selectWorkspace()
	if err != nil || selected != directory {
		t.Fatalf("workspace selection requires no copied path: %q %v", selected, err)
	}
}

func TestGuideCheckPresetsNeedExplicitLocalApproval(t *testing.T) {
	t.Parallel()
	dir := cliWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.invalid/local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, _, calls := testGuide(t, "1\n2\n\n")
	if err := g.runCheck(dir); err != nil || len(*calls) != 0 {
		t.Fatalf("blank approval ran tests: %v %v", err, *calls)
	}
	g, output, calls := testGuide(t, "1\n2\nexecutar\n")
	if err := g.runCheck(dir); err != nil || len(*calls) != 1 {
		t.Fatalf("approved tests not dispatched: %v %v", err, *calls)
	}
	command := (*calls)[0]
	if !contains(command, "20m") || !reflect.DeepEqual(command[len(command)-3:], []string{"go", "test", "./..."}) || !contains(command, dir) {
		t.Fatalf("unexpected test command: %v", command)
	}
	if !strings.Contains(output.String(), "SEM sandbox") {
		t.Fatal("missing execution warning")
	}
}

func TestGuideCustomCommandArgumentsAreLiteral(t *testing.T) {
	t.Parallel()
	g, _, calls := testGuide(t, "1\ncustom-executable\narg with spaces\n$(do-not-execute)\n  significant spaces  \n\n1\nexecutar\n")
	if err := g.runCheck(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	want := []string{"custom-executable", "arg with spaces", "$(do-not-execute)", "  significant spaces  "}
	got := (*calls)[0]
	if !reflect.DeepEqual(got[len(got)-len(want):], want) {
		t.Fatalf("arguments were interpreted/changed: %v", got)
	}
}

func TestGuideWorkspacePublicationStillRequiresReview(t *testing.T) {
	t.Parallel()
	g, _, calls := testGuide(t, "6\n\n0\n")
	if err := g.workspaceMenu(cliWorkspace(t)); !errors.Is(err, errGuideCancelled) || len(*calls) != 0 {
		t.Fatalf("blank review authorized publish: %v %v", err, *calls)
	}
	g, _, calls = testGuide(t, "5\n0\n")
	if err := g.workspaceMenu(cliWorkspace(t)); !errors.Is(err, errGuideCancelled) || len(*calls) != 1 || contains((*calls)[0], "--apply") {
		t.Fatalf("PR preview wrote remote state: %v %v", err, *calls)
	}
}

func TestGuideStagesOnlySelectedReviewedFile(t *testing.T) {
	t.Parallel()
	dir := cliWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("reviewed edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("do not stage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, _, _ := testGuide(t, "1\nrevisado\n")
	if err := g.stageFile(dir); err != nil {
		t.Fatal(err)
	}
	if got := cliGit(t, dir, "diff", "--cached", "--name-only"); got != "base.txt" {
		t.Fatalf("staged unselected work: %s", got)
	}
	if _, err := gitinspect.New().ResolutionTree(context.Background(), dir); err == nil {
		t.Fatal("untracked unreviewed work should still block checks")
	}
}

func TestGuideRunsApprovedSyntheticCheckThroughRealCLI(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("trusted-local checks support Linux only")
	}
	t.Parallel()
	dir := cliWorkspace(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input := "1\n" + executable + "\n-test.run=^TestResolutionCheckHelper$\n--\nsuccess\n\n1\nexecutar\n0\n"
	g, output, _ := testGuide(t, input)
	g.invoke = Run
	if err := g.runCheck(dir); err != nil {
		t.Fatal(err)
	}
	state, err := gitinspect.LoadResolution(dir)
	if err != nil || state.Tree == "" || !strings.Contains(output.String(), "explicit test passed") {
		t.Fatalf("check not verified end-to-end: %+v %v %s", state, err, output)
	}
}

func TestGuideFullResumeReviewStageCheckAndPreviewJourney(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("trusted-local checks support Linux only")
	}
	t.Parallel()
	g, output, _ := testGuide(t, "")
	root := filepath.Join(g.home, "git-worktrees", "api-gateway")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "resolve-main-globex-test")
	if err := os.Rename(cliWorkspace(t), directory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "base.txt"), []byte("edited and reviewed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Root > resolve > resume > workspace > status > diff > stage > select file
	// > review > check > custom executable > args > timeout > trust > preview > exit.
	input := "4\n2\n1\n1\n2\n3\n1\nrevisado\n4\n1\n" + executable + "\n-test.run=^TestResolutionCheckHelper$\n--\nsuccess\n\n1\nexecutar\n5\n0\n0\n"
	g.input = bufferedInput(strings.NewReader(input))
	g.invoke = Run
	if err := g.run(""); !errors.Is(err, errGuideCancelled) {
		t.Fatalf("journey failed: %v\n%s", err, output)
	}
	state, err := gitinspect.New().CheckedResolution(context.Background(), directory)
	if err != nil || state.Tree == "" {
		t.Fatalf("journey did not record a verified tree: %v\n%s", err, output)
	}
	for _, expected := range []string{"api-gateway / main", "Mudanças no índice", "explicit test passed", "PR proposta", "Simulação"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing journey evidence %q:\n%s", expected, output)
		}
	}
	if g.reader.(*guideRepositoryFixture).calls != 0 {
		t.Fatal("local resume/review/check/preview journey should not inventory remotes")
	}
}
