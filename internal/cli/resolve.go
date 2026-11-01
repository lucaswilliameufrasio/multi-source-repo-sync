package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
)

func runResolve(parent context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		return runGuide(parent, []string{"--start", "resolve"}, stdin, stdout, stderr)
	}
	if args[0] == "guide" {
		return runGuide(parent, append([]string{"--start", "resolve"}, args[1:]...), stdin, stdout, stderr)
	}
	if args[0] == "--help" || args[0] == "-h" {
		_, _ = fmt.Fprintln(stderr, "Usage: multi-source-repo-sync resolve <prepare|status|check|publish> [options]")
		return 0
	}
	action := args[0]
	if action != "prepare" && action != "status" && action != "check" && action != "publish" {
		_, _ = fmt.Fprintf(stderr, "unknown resolution action %q\n", action)
		return 2
	}
	flags := flag.NewFlagSet("resolve "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "repository manifest")
	workspace := flags.String("workspace", "", "dedicated persistent resolution checkout")
	timeout := flags.Duration("timeout", 5*time.Minute, "overall timeout")
	var service, branch, target, pairName string
	var apply, reviewed, trusted bool
	if action == "prepare" {
		flags.StringVar(&service, "service", "", "allowlisted service")
		flags.StringVar(&branch, "branch", "", "branch to integrate")
		flags.StringVar(&pairName, "pair", "", "pair of organizations as left/right (required when the service has several pairs)")
		flags.StringVar(&target, "target", "", "label of the organization that receives the PR (required; one of the pair)")
	}
	if action == "check" {
		flags.BoolVar(&trusted, "trusted-local", false, "approve running the explicit command on your host; NOT sandboxed")
	}
	if action == "publish" {
		flags.BoolVar(&apply, "apply", false, "push a new candidate and open a PR after typed confirmation")
		flags.BoolVar(&reviewed, "reviewed", false, "confirm reviewing the entire merge, including organization-specific changes")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *timeout <= 0 || (action != "check" && flags.NArg() != 0) {
		_, _ = fmt.Fprintln(stderr, "invalid timeout or unexpected positional arguments")
		return 2
	}
	if action == "prepare" && service == "" && branch == "" && target == "" && pairName == "" && *workspace == "" {
		return runGuide(parent, []string{"--config", *configPath, "--start", "prepare"}, stdin, stdout, stderr)
	}
	if action == "check" && (!trusted || flags.NArg() == 0) {
		_, _ = fmt.Fprintln(stderr, "check requires --trusted-local -- <executable> [args]. This runs reviewed code with your local permissions; no sandbox or network isolation.")
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "configuration error: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	inspector := gitinspect.New()
	client := github.NewClient(github.ExecRunner{})
	if action == "prepare" {
		if !contains(cfg.Services, service) {
			_, _ = fmt.Fprintf(stderr, "service %q is not in the configured allowlist\n", service)
			return 2
		}
		pair, ok := choosePair(cfg, service, pairName, stderr)
		if !ok {
			return 2
		}
		source, err := otherLabel(pair, target)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
		if err := validateResolutionScope(cfg, service, branch, target, source); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
		if *workspace == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				_, _ = fmt.Fprintln(stderr, err)
				return 1
			}
			*workspace = filepath.Join(home, "git-worktrees", service, "resolve-"+strings.ReplaceAll(branch, "/", "-")+"-"+source+"-to-"+target+"-"+time.Now().UTC().Format("20060102T150405.000000000"))
		}
		if err := validateWorkspacePath(*workspace); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
		if err := os.MkdirAll(filepath.Dir(*workspace), 0o700); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		targetOwner, sourceOwner := resolutionOwners(cfg, target, source)
		left, err := client.ReadRepository(ctx, targetOwner, service, target)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		right, err := client.ReadRepository(ctx, sourceOwner, service, "source")
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		if left.Branches[branch] == "" || right.Branches[branch] == "" || left.Branches[branch] == right.Branches[branch] {
			_, _ = fmt.Fprintln(stderr, "branch missing on one side or already in sync; no workspace created")
			return 1
		}
		state := gitinspect.ResolutionState{Service: service, Branch: branch, Target: target, Source: source, TargetSHA: left.Branches[branch], SourceSHA: right.Branches[branch]}
		status, err := inspector.PrepareResolution(ctx, *workspace, state, github.RepositoryURL(targetOwner, service), github.RepositoryURL(sourceOwner, service))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "prepare failed: %v\nAny partial workspace remains at %s; nothing was pushed.\n", err, *workspace)
			return 1
		}
		return displayResolution(stdout, status, *workspace)
	}
	if *workspace == "" {
		_, _ = fmt.Fprintln(stderr, "--workspace is required to resume a resolution")
		return 2
	}
	state, err := gitinspect.LoadResolution(*workspace)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if err := validateResolutionScope(cfg, state.Service, state.Branch, state.Target, resolutionSource(state)); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if action == "check" || action == "publish" {
		lock, err := os.OpenFile(filepath.Join(*workspace, ".git", "multi-source-resolution.lock"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "workspace locked or inaccessible; another check/publish may be running. Stale locks require manual inspection.")
			return 1
		}
		defer func() {
			_ = lock.Close()
			_ = os.Remove(lock.Name())
		}()
	}
	switch action {
	case "status":
		status, err := inspector.ResolutionStatus(ctx, *workspace)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return displayResolution(stdout, status, *workspace)
	case "check":
		return checkResolution(ctx, inspector, *workspace, flags.Args(), stdout, stderr)
	case "publish":
		return publishResolution(ctx, cfg, inspector, client, *workspace, apply, reviewed, stdin, stdout, stderr)
	}
	return 2
}

// otherLabel is the label of the other organization of the pair; the target must be one of them.
func otherLabel(pair config.Pair, target string) (string, error) {
	switch target {
	case pair.Left.Label:
		return pair.Right.Label, nil
	case pair.Right.Label:
		return pair.Left.Label, nil
	default:
		return "", fmt.Errorf("--target must be %s or %s; the CLI never chooses which side wins", pair.Left.Label, pair.Right.Label)
	}
}

// resolutionSource is where the changes of a resolution come from.
func resolutionSource(state gitinspect.ResolutionState) string {
	return state.Source
}

func validateResolutionScope(cfg config.Config, service string, branch string, target string, source string) error {
	if !contains(cfg.Services, service) || !contains(cfg.SyncBranches, branch) || contains(cfg.PreserveOnlyBranches, branch) || branch == "dev-test" {
		return fmt.Errorf("service/branch is not an allowed synchronization candidate")
	}
	targetOrg, targetOK := cfg.Organizations.Find(target)
	sourceOrg, sourceOK := cfg.Organizations.Find(source)
	if !targetOK || !sourceOK || target == source {
		return fmt.Errorf("--target and its source must be two different organizations of the manifest; the CLI never chooses which side wins")
	}
	if _, ok := cfg.Pair(targetOrg.Label + "/" + sourceOrg.Label); !ok {
		return fmt.Errorf("%s and %s are not a configured pair", target, source)
	}
	if !cfg.HasService(target, service) || !cfg.HasService(source, service) {
		return fmt.Errorf("service %q has no copy in both %s and %s", service, target, source)
	}
	return nil
}

func resolutionOwners(cfg config.Config, target string, source string) (string, string) {
	targetOrg, _ := cfg.Organizations.Find(target)
	sourceOrg, _ := cfg.Organizations.Find(source)
	return targetOrg.Owner, sourceOrg.Owner
}

func validateWorkspacePath(directory string) error {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	// Resolve existing ancestors as well, so a symlink cannot hide a temp path.
	probe := absolute
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for _, root := range []string{"/tmp", "/var/tmp", "/dev/shm", "/run", os.TempDir()} {
				if root != "" && (resolved == root || strings.HasPrefix(resolved, root+string(filepath.Separator))) {
					return fmt.Errorf("choose a persistent workspace, such as ~/git-worktrees; temporary paths are not supported")
				}
			}
			return nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fmt.Errorf("cannot resolve workspace parent")
		}
		probe = parent
	}
}

func displayResolution(writer io.Writer, status gitinspect.ResolutionStatus, directory string) int {
	var output strings.Builder
	fmt.Fprintf(&output, "%s / %s → %s\nWorkspace: %s\n", sanitizeTerminalText(status.State.Service), sanitizeTerminalText(status.State.Branch), sanitizeTerminalText(status.State.Target), sanitizeTerminalText(directory))
	if len(status.Conflicts) > 0 {
		fmt.Fprintf(&output, "\n%d arquivos precisam de decisão:\n", len(status.Conflicts))
		for _, path := range status.Conflicts {
			fmt.Fprintf(&output, "  ! %s\n", sanitizeTerminalText(path))
		}
		fmt.Fprintln(&output, "\nO Git combinou os trechos não conflitantes; nenhum lado foi escolhido automaticamente.")
		fmt.Fprintln(&output, "Revise git diff e git status no workspace, edite os conflitos e use git add nos arquivos revisados.")
	} else {
		fmt.Fprintln(&output, "\nSem entradas de conflito no índice. Revise todo o diff antes de testar/publicar.")
	}
	if status.State.Tree != "" {
		fmt.Fprintln(&output, "Há um check registrado; publish revalida se corresponde à versão atual.")
	}
	fmt.Fprintf(&output, "Continuar pelo menu: multi-source-repo-sync guide --workspace %s\n", shellArgument(directory))
	fmt.Fprintln(&output, "Nada foi mesclado nas branches base. publish é simulação sem --apply.")
	if _, err := io.WriteString(writer, output.String()); err != nil {
		return 1
	}
	return 0
}

func checkResolution(ctx context.Context, inspector gitinspect.GitInspector, directory string, command []string, stdout io.Writer, stderr io.Writer) int {
	tree, err := inspector.ResolutionTree(ctx, directory)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	state, err := gitinspect.LoadResolution(directory)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	// A failed retry must not leave an older successful check eligible for publish.
	state.Tree, state.Check, state.Candidate = "", nil, ""
	if err := gitinspect.SaveResolution(directory, state); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err := fmt.Fprintln(stderr, "Execução local autorizada, SEM sandbox; somente o comando fornecido será executado."); err != nil {
		return 1
	}
	if err := runLocalCheck(ctx, directory, command, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "check failed: %v; publication remains blocked\n", err)
		return 1
	}
	if err := inspector.RecordCheck(ctx, directory, tree, command); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, err = fmt.Fprintf(stdout, "Check passou na versão exata revisada.\nContinuar pelo menu: multi-source-repo-sync guide --workspace %s\n", shellArgument(directory))
	if err != nil {
		return 1
	}
	return 0
}

type resolutionPublisher interface {
	MergeCommitAllowed(context.Context, string, string) (bool, error)
	EnsurePullRequest(context.Context, string, string, string, string, string, string) (github.PullRequestResult, error)
}

func publishResolution(ctx context.Context, cfg config.Config, inspector gitinspect.GitInspector, client resolutionPublisher, directory string, apply bool, reviewed bool, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	state, err := inspector.CheckedResolution(ctx, directory)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	targetOwner, sourceOwner := resolutionOwners(cfg, state.Target, resolutionSource(state))
	if _, err := fmt.Fprintf(stdout, "PR proposta: %s/%s → %s\nPreserva os históricos de %s e %s. Nenhuma base será alterada.\n",
		targetOwner, state.Service, state.Branch, targetOwner, sourceOwner); err != nil {
		return 1
	}
	if !apply {
		_, _ = fmt.Fprintln(stdout, "Simulação. Revise o diff completo e os testes; para publicar, use --apply --reviewed.")
		return 0
	}
	if !reviewed {
		_, _ = fmt.Fprintln(stderr, "--reviewed required: review all changes, including organization-specific code and test coverage")
		return 2
	}
	phrase := "publish " + state.Service + " " + state.Branch + " " + resolutionSource(state) + "-to-" + state.Target
	if stdin == nil {
		_, _ = fmt.Fprintln(stderr, "confirmation input is required")
		return 2
	}
	if _, err := fmt.Fprintf(stderr, "Digite %q para criar a branch e PR (pode iniciar CI): ", phrase); err != nil {
		return 1
	}
	line, err := bufferedInput(stdin).ReadString('\n')
	if err != nil || strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r") != phrase {
		_, _ = fmt.Fprintln(stderr, "Cancelado. Nada enviado.")
		return 0
	}
	allowed, err := client.MergeCommitAllowed(ctx, targetOwner, state.Service)
	if err != nil || !allowed {
		_, _ = fmt.Fprintln(stderr, "target merge-commit support unavailable or disabled; nothing pushed")
		return 1
	}
	candidate, err := inspector.PushResolution(ctx, directory, github.RepositoryURL(targetOwner, state.Service), github.RepositoryURL(sourceOwner, state.Service))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "publication stopped: %v\nAny candidate remains for inspection/retry; no branch was deleted.\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "Branch candidata: %s\n", candidate.BranchName)
	body := fmt.Sprintf("Human-reviewed conflict resolution. Both original histories are preserved.\n\nTarget: %s\nSource: %s\nChecked tree: %s\nCheck identity (executable and argument digest): %q\n\nUse a merge commit, not squash or rebase. No automatic merge. Test success is not proof of semantic equivalence.", state.TargetSHA, state.SourceSHA, state.Tree, state.Check)
	pr, err := client.EnsurePullRequest(ctx, targetOwner, state.Service, candidate.BranchName, state.Branch, "Resolve sync conflicts for "+state.Service+" "+state.Branch, body)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "PR failed: %v; candidate preserved, retry the same publish command\n", err)
		return 1
	}
	_, err = fmt.Fprintf(stdout, "PR: %s\nNada foi mesclado automaticamente.\n", pr.URL)
	if err != nil {
		return 1
	}
	return 0
}
