package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/app"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/domain"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/syncer"
)

// Version is set at build time (-ldflags "-X .../internal/cli.Version=1.2.3").
var Version = "dev"

func Run(parent context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		return runGuide(parent, nil, stdin, stdout, stderr)
	}
	if args[0] == "guide" {
		return runGuide(parent, args[1:], stdin, stdout, stderr)
	}
	if args[0] == "version" || args[0] == "--version" {
		_, _ = fmt.Fprintf(stdout, "multi-source-repo-sync %s\n", Version)
		return 0
	}
	if args[0] == "propagate" {
		return runPropagate(parent, args[1:], stdin, stdout, stderr)
	}
	if args[0] == "serve" {
		return runServe(parent, args[1:], stdout, stderr)
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if err := printUsage(stdout); err != nil {
			return 1
		}
		return 0
	}
	if len(args) > 0 && args[0] == "resolve" {
		return runResolve(parent, args[1:], stdin, stdout, stderr)
	}

	mode := args[0]
	if mode != "status" && mode != "plan" && mode != "sync" {
		if _, err := fmt.Fprintf(stderr, "unknown command %q\n", mode); err != nil {
			return 1
		}
		if err := printUsage(stderr); err != nil {
			return 1
		}
		return 2
	}

	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to repository configuration")
	outputFormat := flags.String("format", "text", "output format: text or json")
	timeout := flags.Duration("timeout", 90*time.Second, "overall request timeout")
	service := flags.String("service", "", "repository service name (sync only)")
	pairName := flags.String("pair", "", "pair of organizations as left/right (status and plan: only that pair; sync: required when the service has several pairs)")
	branch := flags.String("branch", "", "branch to synchronize (sync only)")
	apply := flags.Bool("apply", false, "create new sync branches and pull requests (sync only; requires typed confirmation)")
	verbose := flags.Bool("verbose", false, "show full branch inventory, hashes, and pull request details")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		if _, err := fmt.Fprintf(stderr, "unexpected positional arguments: %s\n", strings.Join(flags.Args(), " ")); err != nil {
			return 1
		}
		return 2
	}
	if *outputFormat != "text" && *outputFormat != "json" {
		if _, err := fmt.Fprintf(stderr, "unsupported output format %q; use text or json\n", *outputFormat); err != nil {
			return 1
		}
		return 2
	}
	if *timeout <= 0 {
		if _, err := fmt.Fprintln(stderr, "timeout must be greater than zero"); err != nil {
			return 1
		}
		return 2
	}
	if mode == "sync" {
		if *service == "" && *branch == "" && !*apply && *outputFormat == "text" {
			return runGuide(parent, []string{"--config", *configPath, "--start", "sync"}, stdin, stdout, stderr)
		}
		if *outputFormat != "text" {
			if _, err := fmt.Fprintln(stderr, "sync supports text output only"); err != nil {
				return 1
			}
			return 2
		}
		if *service == "" || *branch == "" {
			if _, err := fmt.Fprintln(stderr, "sync requires both --service and --branch"); err != nil {
				return 1
			}
			return 2
		}
	} else if *service != "" || *branch != "" || *apply {
		if _, err := fmt.Fprintln(stderr, "--service, --branch, and --apply are only valid with sync"); err != nil {
			return 1
		}
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "configuration error: %v\n", err); writeErr != nil {
			return 1
		}
		return 2
	}

	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()

	reader := github.NewClient(github.ExecRunner{})
	inspector := gitinspect.New()
	if mode == "sync" {
		return runSync(ctx, cfg, reader, inspector, *service, *branch, *pairName, *apply, *verbose, stdin, stdout, stderr)
	}
	if *pairName != "" {
		pair, ok := cfg.Pair(*pairName)
		if !ok {
			if _, err := fmt.Fprintf(stderr, "pair %q is not configured; the pairs are: %s\n", *pairName, pairList(cfg)); err != nil {
				return 1
			}
			return 2
		}
		cfg = scopeToPair(cfg, pair)
	}
	reporter := app.Reporter{Reader: reader, Inspector: inspector}
	report, err := reporter.Build(ctx, cfg, mode)
	if err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "build report: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}

	if *outputFormat == "json" {
		if err := writeJSON(stdout, report); err != nil {
			if _, writeErr := fmt.Fprintf(stderr, "write report: %v\n", err); writeErr != nil {
				return 1
			}
			return 1
		}
	} else {
		if err := writeReportText(stdout, report, *verbose); err != nil {
			if _, writeErr := fmt.Fprintf(stderr, "write report: %v\n", err); writeErr != nil {
				return 1
			}
			return 1
		}
	}

	if hasErrors(report) {
		return 1
	}
	return 0
}

func printUsage(writer io.Writer) error {
	if _, err := fmt.Fprintln(writer, "Usage: multi-source-repo-sync [guide|status|plan|sync|propagate|resolve|serve] [options]\nSem argumentos: abre o guia interativo com as opções disponíveis."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, "propagate puts one branch of one organization into the other copies and opens a pull request into each (dry run unless --apply)."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, "serve opens the same status, plan and sync as a local web page (loopback only, with an access token)."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, "status and plan are read-only. sync is a dry run by default; --apply creates new branches and PRs after typed confirmation."); err != nil {
		return err
	}
	return nil
}

func runSync(
	ctx context.Context,
	cfg config.Config,
	reader github.Client,
	inspector gitinspect.GitInspector,
	service string,
	branchName string,
	pairName string,
	apply bool,
	verbose bool,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if !contains(cfg.Services, service) {
		if _, err := fmt.Fprintf(stderr, "service %q is not in the configured allowlist\n", service); err != nil {
			return 1
		}
		return 2
	}
	if !contains(cfg.SyncBranches, branchName) || contains(cfg.PreserveOnlyBranches, branchName) {
		if _, err := fmt.Fprintf(stderr, "branch %q is not configured as a sync candidate\n", branchName); err != nil {
			return 1
		}
		return 2
	}

	pair, ok := choosePair(cfg, service, pairName, stderr)
	if !ok {
		return 2
	}
	scopedConfig := scopeToPair(cfg, pair)
	scopedConfig.Services = []string{service}
	reporter := app.Reporter{Reader: reader, Inspector: inspector}
	report, err := reporter.Build(ctx, scopedConfig, "plan")
	if err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "build plan: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := writeReportText(stdout, report, verbose); err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "write plan: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if hasErrors(report) {
		return 1
	}
	if len(report.Repositories) != 1 {
		if _, err := fmt.Fprintln(stderr, "expected exactly one repository in the scoped plan"); err != nil {
			return 1
		}
		return 1
	}

	repository := report.Repositories[0]
	branch, ok := findBranch(repository, branchName)
	if !ok {
		if _, err := fmt.Fprintf(stderr, "branch %q was not found in the plan\n", branchName); err != nil {
			return 1
		}
		return 1
	}
	if branch.Assessment != "clean_merge" {
		if _, err := fmt.Fprintf(stdout, "No sync applied: %s (%s).\n", branch.Assessment, branch.Action); err != nil {
			return 1
		}
		if branch.Assessment == "merge_conflict" {
			if _, err := fmt.Fprintf(stdout, "Resolução assistida: multi-source-repo-sync resolve prepare --service %s --branch %s --pair %s --target %s\nEscolha o alvo da PR explicitamente: %s ou %s. Isso só prepara uma cópia local.\n", service, branchName, pair.Name(), pair.Right.Label, pair.Left.Label, pair.Right.Label); err != nil {
				return 1
			}
		}
		return 1
	}
	if err := printProposedWrites(stdout, repository, branch); err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "write proposed actions: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := checkMergeCommitSupport(ctx, repository, branch, reader, stdout); err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "sync blocked: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if !apply {
		if _, err := fmt.Fprintln(stdout, "Dry run only. No remote branches or pull requests were changed."); err != nil {
			return 1
		}
		return 0
	}

	confirmed, err := confirmApply(stdin, stderr, service, branchName, pair.Name())
	if err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "read confirmation: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if !confirmed {
		if _, err := fmt.Fprintln(stderr, "Cancelled; no remote branches or pull requests were changed."); err != nil {
			return 1
		}
		return 0
	}

	freshReport, err := reporter.Build(ctx, scopedConfig, "plan")
	if err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "recheck plan: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if hasErrors(freshReport) || !sameBranchPlan(report, freshReport, branchName) {
		if _, err := fmt.Fprintln(stderr, "The branch heads or plan changed after confirmation; no sync was applied. Review a fresh plan and try again."); err != nil {
			return 1
		}
		return 1
	}

	freshRepository := freshReport.Repositories[0]
	freshBranch, _ := findBranch(freshRepository, branchName)
	executor := syncer.Executor{Git: inspector, PRs: reader, MergeChecker: reader}
	results, err := executor.Apply(ctx, freshRepository, freshBranch)
	for _, result := range results {
		if _, writeErr := fmt.Fprintf(
			stdout,
			"%s: candidate %s (%s); branch_created=%t; PR=%s; pr_created=%t\n",
			result.TargetSide,
			result.Candidate,
			shortSHA(result.CommitSHA),
			result.BranchCreated,
			result.PullRequest,
			result.PRCreated,
		); writeErr != nil {
			return 1
		}
	}
	if err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "sync stopped: %v\nNo existing branch was overwritten or deleted. Any reported candidate branch is left in place for review or a safe retry.\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	return 0
}

func printProposedWrites(writer io.Writer, repository domain.RepositoryStatus, branch domain.BranchStatus) error {
	for _, targetSide := range branch.PRTargets {
		target, source, targetSHA, sourceSHA, err := syncSides(repository, branch, targetSide)
		if err != nil {
			return err
		}
		candidate := syncer.CandidateBranchName(targetSide, source.Label, branch.Name, targetSHA, sourceSHA)
		if _, err := fmt.Fprintf(writer, "Would push new branch %s to %s/%s and open a PR to %s.\n", candidate, target.Owner, target.Repository, branch.Name); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(writer, "  Source history: %s/%s %s at %s.\n", source.Owner, source.Repository, branch.Name, shortSHA(sourceSHA)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(writer, "Apply never force-pushes, updates existing candidate branches, deletes branches, or merges PRs. Opening PRs may trigger GitHub checks.")
	return err
}

type mergeCommitChecker interface {
	MergeCommitAllowed(ctx context.Context, owner string, repository string) (bool, error)
}

func checkMergeCommitSupport(ctx context.Context, repository domain.RepositoryStatus, branch domain.BranchStatus, checker mergeCommitChecker, writer io.Writer) error {
	for _, targetSide := range branch.PRTargets {
		target, _, _, _, err := syncSides(repository, branch, targetSide)
		if err != nil {
			return err
		}
		allowed, err := checker.MergeCommitAllowed(ctx, target.Owner, target.Repository)
		if err != nil {
			return fmt.Errorf("read merge-commit setting for %s/%s: %w", target.Owner, target.Repository, err)
		}
		if _, err := fmt.Fprintf(writer, "%s/%s: allow_merge_commit=%t\n", target.Owner, target.Repository, allowed); err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("%s/%s does not allow merge commits; no branch was pushed", target.Owner, target.Repository)
		}
	}
	return nil
}

func syncSides(repository domain.RepositoryStatus, branch domain.BranchStatus, targetLabel string) (domain.RepositorySide, domain.RepositorySide, string, string, error) {
	switch targetLabel {
	case repository.Left.Label:
		return repository.Left, repository.Right, branch.LeftSHA, branch.RightSHA, nil
	case repository.Right.Label:
		return repository.Right, repository.Left, branch.RightSHA, branch.LeftSHA, nil
	default:
		return domain.RepositorySide{}, domain.RepositorySide{}, "", "", fmt.Errorf("unknown PR target %q", targetLabel)
	}
}

// scopeToPair makes the configuration list only this pair.
func scopeToPair(cfg config.Config, pair config.Pair) config.Config {
	scoped := cfg
	scoped.PairLabels = [][]string{{pair.Left.Label, pair.Right.Label}}
	return scoped
}

func pairList(cfg config.Config) string {
	names := make([]string, 0)
	for _, pair := range cfg.Pairs() {
		names = append(names, pair.Name())
	}
	return strings.Join(names, ", ")
}

// choosePair picks the pair a sync works on. It must be given when the service has several.
func choosePair(cfg config.Config, service string, pairName string, stderr io.Writer) (config.Pair, bool) {
	available := cfg.PairsFor(service)
	if pairName != "" {
		pair, ok := cfg.Pair(pairName)
		if !ok {
			_, _ = fmt.Fprintf(stderr, "pair %q is not configured; the pairs are: %s\n", pairName, pairList(cfg))
			return config.Pair{}, false
		}
		for _, candidate := range available {
			if candidate.Name() == pair.Name() {
				return pair, true
			}
		}
		_, _ = fmt.Fprintf(stderr, "service %q has no copy in both organizations of %s\n", service, pair.Name())
		return config.Pair{}, false
	}
	if len(available) == 1 {
		return available[0], true
	}
	names := make([]string, 0, len(available))
	for _, pair := range available {
		names = append(names, pair.Name())
	}
	_, _ = fmt.Fprintf(stderr, "service %q is in several pairs; choose one with --pair: %s\n", service, strings.Join(names, ", "))
	return config.Pair{}, false
}

func confirmApply(reader io.Reader, writer io.Writer, service string, branch string, pair string) (bool, error) {
	if reader == nil {
		return false, fmt.Errorf("confirmation input is required")
	}
	confirmation := "sync " + service + " " + branch + " " + pair
	if _, err := fmt.Fprintf(writer, "Type %q to create the listed candidate branches and PRs: ", confirmation); err != nil {
		return false, err
	}
	line, err := bufferedInput(reader).ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line == confirmation, nil
}

func sameBranchPlan(first domain.Report, second domain.Report, branchName string) bool {
	if len(first.Repositories) != 1 || len(second.Repositories) != 1 {
		return false
	}
	firstBranch, firstOK := findBranch(first.Repositories[0], branchName)
	secondBranch, secondOK := findBranch(second.Repositories[0], branchName)
	if !firstOK || !secondOK {
		return false
	}
	if firstBranch.Assessment != "clean_merge" || secondBranch.Assessment != "clean_merge" {
		return false
	}
	if firstBranch.LeftSHA != secondBranch.LeftSHA || firstBranch.RightSHA != secondBranch.RightSHA {
		return false
	}
	if len(firstBranch.PRTargets) != len(secondBranch.PRTargets) {
		return false
	}
	for index := range firstBranch.PRTargets {
		if firstBranch.PRTargets[index] != secondBranch.PRTargets[index] {
			return false
		}
	}
	return true
}

func findBranch(repository domain.RepositoryStatus, branchName string) (domain.BranchStatus, bool) {
	for _, branch := range repository.Branches {
		if branch.Name == branchName {
			return branch, true
		}
	}
	return domain.BranchStatus{}, false
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func writeJSON(writer io.Writer, report domain.Report) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	return nil
}

func writeText(writer io.Writer, report domain.Report) error {
	return writeReportText(writer, report, false)
}

func writeReportText(writer io.Writer, report domain.Report, verbose bool) error {
	if verbose {
		return writeVerboseText(writer, report)
	}
	var output strings.Builder
	fmt.Fprintf(&output, "Sincronização entre organizações | %s | %d pares de serviço\n", report.Mode, len(report.Repositories))
	var branches, conflicts, ready, synced, unavailable int
	for _, repository := range report.Repositories {
		fmt.Fprintf(&output, "\n%s  (%s ↔ %s)\n", sanitizeTerminalText(repository.Service), sanitizeTerminalText(repository.Left.Label), sanitizeTerminalText(repository.Right.Label))
		for _, branch := range repository.Branches {
			if branch.Policy != "sync_candidate" {
				continue
			}
			branches++
			switch branch.Assessment {
			case "in_sync":
				synced++
			case "merge_conflict", "unrelated_histories":
				conflicts++
			case "clean_merge":
				ready++
			case "repository_unavailable", "history_unavailable":
				unavailable++
			}
			fmt.Fprintf(&output, "  %-8s %s\n", sanitizeTerminalText(branch.Name), branchSummary(repository, branch))
		}
		for _, message := range repository.Errors {
			fmt.Fprintf(&output, "  ERRO: %s\n", sanitizeTerminalText(message))
		}
		prs := len(repository.Left.OpenPullRequests) + len(repository.Right.OpenPullRequests)
		if prs > 0 {
			fmt.Fprintf(&output, "  PRs abertas: %d %s · %d %s (ver --verbose)\n",
				len(repository.Left.OpenPullRequests), sanitizeTerminalText(repository.Left.Label),
				len(repository.Right.OpenPullRequests), sanitizeTerminalText(repository.Right.Label))
		}
	}
	if report.Mode == "status" {
		fmt.Fprintf(&output, "\n%d branches: %d iguais · %d diferentes, ausentes ou indisponíveis\n",
			branches, synced, branches-synced)
		fmt.Fprintln(&output, "Próximo: multi-source-repo-sync plan (verifica histórico e conflitos).")
	} else {
		fmt.Fprintf(&output, "\n%d branches: %d iguais · %d combináveis · %d bloqueadas\n",
			branches, synced, ready, conflicts)
		fmt.Fprintf(&output, "%d sem diagnóstico · %d diferentes ou ausentes (ver acima)\n",
			unavailable, branches-synced-ready-conflicts-unavailable)
		fmt.Fprintln(&output, "Combinável não significa aprovado: revise as mudanças antes de aplicar.")
		if conflicts > 0 {
			fmt.Fprintln(&output, "Conflitos: use resolve prepare --help para iniciar uma resolução assistida.")
		}
	}
	fmt.Fprintln(&output, "Somente leitura. Inventário completo: --verbose · Dados estruturados: --format json")
	_, err := io.WriteString(writer, output.String())
	return err
}

func branchSummary(repository domain.RepositoryStatus, branch domain.BranchStatus) string {
	switch branch.Assessment {
	case "in_sync":
		return "OK — em sincronia"
	case "different_heads":
		return "DIFERENTE — rode plan para verificar"
	case "clean_merge":
		return "COMBINÁVEL — PR para " + strings.Join(branch.PRTargets, ", ")
	case "merge_conflict":
		return "CONFLITO — resolver antes de sincronizar"
	case "unrelated_histories":
		return "BLOQUEADA — históricos sem ancestral comum"
	case "missing_on_both":
		return "— não existe nos dois lados"
	case "repository_unavailable", "history_unavailable":
		return "ERRO — diagnóstico indisponível"
	case "missing_on_" + repository.Left.Label:
		return "SÓ " + strings.ToUpper(sanitizeTerminalText(repository.Right.Label)) + " — preservada, sem criar contraparte"
	case "missing_on_" + repository.Right.Label:
		return "SÓ " + strings.ToUpper(sanitizeTerminalText(repository.Left.Label)) + " — preservada, sem criar contraparte"
	default:
		return sanitizeTerminalText(branch.Assessment)
	}
}

func writeVerboseText(writer io.Writer, report domain.Report) error {
	if _, err := fmt.Fprintf(writer, "Organization sync %s report\n", report.Mode); err != nil {
		return err
	}
	for _, repository := range report.Repositories {
		if _, err := fmt.Fprintf(
			writer,
			"\n%s [%s]: %s %d branches/%d open PRs; %s %d branches/%d open PRs\n",
			repository.Service,
			repository.Pair,
			repository.Left.Label,
			len(repository.Left.Branches),
			len(repository.Left.OpenPullRequests),
			repository.Right.Label,
			len(repository.Right.Branches),
			len(repository.Right.OpenPullRequests),
		); err != nil {
			return err
		}
		if repository.Left.Error != "" {
			if _, err := fmt.Fprintf(writer, "  %s unavailable: %s\n", repository.Left.Label, repository.Left.Error); err != nil {
				return err
			}
		}
		if repository.Right.Error != "" {
			if _, err := fmt.Fprintf(writer, "  %s unavailable: %s\n", repository.Right.Label, repository.Right.Error); err != nil {
				return err
			}
		}

		for _, branch := range repository.Branches {
			if _, err := fmt.Fprintf(
				writer,
				"  %-24s [%-14s] %s=%-12s %s=%-12s %-22s %s",
				branch.Name,
				branch.Policy,
				repository.Left.Label,
				shortSHA(branch.LeftSHA),
				repository.Right.Label,
				shortSHA(branch.RightSHA),
				branch.Assessment,
				branch.Action,
			); err != nil {
				return err
			}
			if len(branch.PRTargets) > 0 {
				if _, err := fmt.Fprintf(writer, " (PR target: %s)", strings.Join(branch.PRTargets, ", ")); err != nil {
					return err
				}
			}
			if branch.Reason != "" {
				if _, err := fmt.Fprintf(writer, " — %s", branch.Reason); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(writer); err != nil {
				return err
			}
		}

		pullRequests := append([]domain.PullRequest(nil), repository.Left.OpenPullRequests...)
		pullRequests = append(pullRequests, repository.Right.OpenPullRequests...)
		sort.Slice(pullRequests, func(i int, j int) bool {
			if pullRequests[i].Side == pullRequests[j].Side {
				return pullRequests[i].Number < pullRequests[j].Number
			}
			return pullRequests[i].Side < pullRequests[j].Side
		})
		for _, pullRequest := range pullRequests {
			if _, err := fmt.Fprintf(
				writer,
				"  PR %s #%d %s -> %s: %s%s\n",
				pullRequest.Side,
				pullRequest.Number,
				pullRequest.HeadBranch,
				pullRequest.BaseBranch,
				sanitizeTerminalText(pullRequest.Title),
				formatDraft(pullRequest.Draft),
			); err != nil {
				return err
			}
		}

		for _, message := range repository.Errors {
			if _, err := fmt.Fprintf(writer, "  ERROR: %s\n", message); err != nil {
				return err
			}
		}
	}
	return nil
}

func shortSHA(value string) string {
	if value == "" {
		return "-"
	}
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func formatDraft(draft bool) string {
	if draft {
		return " [draft]"
	}
	return ""
}

func sanitizeTerminalText(value string) string {
	characters := []rune(value)
	var sanitized strings.Builder
	for index := 0; index < len(characters); index++ {
		character := characters[index]
		if character == '\x1b' {
			if index+1 < len(characters) && characters[index+1] == '[' {
				index += 2
				for index < len(characters) {
					if characters[index] >= '@' && characters[index] <= '~' {
						break
					}
					index++
				}
				continue
			}
			if index+1 < len(characters) {
				index++
			}
			continue
		}
		if character < 0x20 || character == 0x7f {
			sanitized.WriteRune(' ')
			continue
		}
		sanitized.WriteRune(character)
	}
	return strings.Join(strings.Fields(sanitized.String()), " ")
}

func hasErrors(report domain.Report) bool {
	for _, repository := range report.Repositories {
		if len(repository.Errors) > 0 {
			return true
		}
	}
	return false
}
