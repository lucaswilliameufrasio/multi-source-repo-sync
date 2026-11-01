package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
)

var errGuideCancelled = errors.New("guia encerrado; nenhuma ação adicional autorizada")

func bufferedInput(reader io.Reader) *bufio.Reader {
	if buffered, ok := reader.(*bufio.Reader); ok {
		return buffered
	}
	if reader == nil {
		reader = strings.NewReader("")
	}
	return bufio.NewReader(reader)
}

type guide struct {
	ctx        context.Context
	cfg        config.Config
	configPath string
	home       string
	input      *bufio.Reader
	output     io.Writer
	errors     io.Writer
	reader     github.RepositoryReader
	invoke     func(context.Context, []string, io.Reader, io.Writer, io.Writer) int
}

func runGuide(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("guide", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "repository manifest")
	start := flags.String("start", "", "start at sync, prepare, or resolve")
	workspace := flags.String("workspace", "", "resume this workspace directly in the guided menu")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || (*start != "" && *start != "sync" && *start != "prepare" && *start != "resolve") {
		_, _ = fmt.Fprintln(stderr, "invalid guide options; use guide --help")
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "configuration error: %v\n", err)
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	g := guide{ctx: ctx, cfg: cfg, configPath: *configPath, home: home, input: bufferedInput(stdin), output: stdout, errors: stderr,
		reader: github.NewClient(github.ExecRunner{}), invoke: Run}
	var guideErr error
	if *workspace != "" {
		guideErr = g.workspaceMenu(*workspace)
	} else {
		guideErr = g.run(*start)
	}
	if guideErr != nil && !errors.Is(guideErr, errGuideCancelled) {
		_, _ = fmt.Fprintln(stderr, guideErr)
		return 1
	}
	return 0
}

func shellArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (g *guide) line(prompt string) (string, error) {
	line, err := g.literalLine(prompt)
	return strings.TrimSpace(line), err
}

func (g *guide) literalLine(prompt string) (string, error) {
	if err := g.ctx.Err(); err != nil {
		return "", err
	}
	if _, err := fmt.Fprint(g.output, prompt); err != nil {
		return "", err
	}
	line, err := g.input.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", errGuideCancelled
		}
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

// Blank input, zero and EOF always cancel; no option has an implicit default.
func (g *guide) choose(title string, options []string) (int, error) {
	if _, err := fmt.Fprintf(g.output, "\n%s\n", title); err != nil {
		return 0, err
	}
	for index, option := range options {
		if _, err := fmt.Fprintf(g.output, "  %d) %s\n", index+1, sanitizeTerminalText(option)); err != nil {
			return 0, err
		}
	}
	if _, err := fmt.Fprintln(g.output, "  0) Voltar / sair (sem executar nada)"); err != nil {
		return 0, err
	}
	for {
		line, err := g.line("Escolha o número: ")
		if err != nil {
			return 0, err
		}
		if line == "" || line == "0" {
			return 0, errGuideCancelled
		}
		choice, err := strconv.Atoi(line)
		if err == nil && choice > 0 && choice <= len(options) {
			return choice - 1, nil
		}
		if _, err := fmt.Fprintf(g.output, "Opção inválida. Use 1 a %d ou 0 para sair.\n", len(options)); err != nil {
			return 0, err
		}
	}
}

func (g *guide) approve(prompt string, phrase string) (bool, error) {
	line, err := g.line(prompt + " Digite " + strconv.Quote(phrase) + " para autorizar; Enter cancela: ")
	if err != nil {
		return false, err
	}
	return line == phrase, nil
}

func (g *guide) call(args ...string) int {
	// All modes accept --config after their command (resolve has a subcommand).
	prefix := 1
	if args[0] == "resolve" {
		prefix = 2
	}
	command := append([]string(nil), args[:prefix]...)
	command = append(command, "--config", g.configPath)
	command = append(command, args[prefix:]...)
	return g.invoke(g.ctx, command, g.input, g.output, g.errors)
}

func (g *guide) run(start string) error {
	if _, err := fmt.Fprintln(g.output, "Sincronização entre organizações — guia de uso\nEscolher uma opção não autoriza testes, push ou merge automaticamente."); err != nil {
		return err
	}
	if start == "sync" {
		return g.newFlow(false)
	}
	if start == "prepare" {
		return g.newFlow(true)
	}
	if start == "resolve" {
		return g.resolutionMenu()
	}
	for {
		choice, err := g.choose("O que você quer fazer?", []string{
			"Ver situação dos repositórios — só leitura",
			"Analisar diferenças e conflitos — só leitura",
			"Sincronizar um serviço — começar por simulação",
			"Resolver conflitos — criar ou retomar uma cópia local",
		})
		if err != nil {
			return err
		}
		switch choice {
		case 0:
			g.call("status")
		case 1:
			g.call("plan")
		case 2:
			err = g.newFlow(false)
		case 3:
			err = g.resolutionMenu()
		}
		if err != nil && !errors.Is(err, errGuideCancelled) {
			return err
		}
	}
}

func (g *guide) resolutionMenu() error {
	choice, err := g.choose("Resolução assistida", []string{
		"Preparar uma nova resolução — nenhum push",
		"Retomar uma resolução existente — listar workspaces",
	})
	if err != nil {
		return err
	}
	if choice == 0 {
		return g.newFlow(true)
	}
	directory, err := g.selectWorkspace()
	if err != nil {
		return err
	}
	return g.workspaceMenu(directory)
}

func (g *guide) selectBranch() (string, config.Pair, string, error) {
	selected, err := g.choose("Qual serviço? (lista do manifesto)", g.cfg.Services)
	if err != nil {
		return "", config.Pair{}, "", err
	}
	service := g.cfg.Services[selected]
	pairs := g.cfg.PairsFor(service)
	if len(pairs) == 0 {
		_, err := fmt.Fprintln(g.output, "Esse serviço não tem cópia em duas organizações do manifesto.")
		return "", config.Pair{}, "", err
	}
	pair := pairs[0]
	if len(pairs) > 1 {
		names := make([]string, 0, len(pairs))
		for _, candidate := range pairs {
			names = append(names, candidate.Left.Label+" ↔ "+candidate.Right.Label)
		}
		index, err := g.choose("Entre quais organizações? Cada par é sincronizado separadamente.", names)
		if err != nil {
			return "", config.Pair{}, "", err
		}
		pair = pairs[index]
	}
	if _, err := fmt.Fprintln(g.output, "Consultando branches nos dois lados; nada será alterado..."); err != nil {
		return "", config.Pair{}, "", err
	}
	ctx, cancel := context.WithTimeout(g.ctx, 90*time.Second)
	defer cancel()
	left, err := g.reader.ReadRepository(ctx, pair.Left.Owner, service, pair.Left.Label)
	if err != nil {
		return "", config.Pair{}, "", fmt.Errorf("não foi possível consultar %s; confira autenticação gh e rede: %w", pair.Left.Label, err)
	}
	right, err := g.reader.ReadRepository(ctx, pair.Right.Owner, service, pair.Right.Label)
	if err != nil {
		return "", config.Pair{}, "", fmt.Errorf("não foi possível consultar %s; confira autenticação gh e rede: %w", pair.Right.Label, err)
	}
	var branches []string
	for _, name := range g.cfg.SyncBranches {
		if contains(g.cfg.PreserveOnlyBranches, name) || name == "dev-test" {
			continue
		}
		if left.Branches[name] != "" && right.Branches[name] != "" && left.Branches[name] != right.Branches[name] {
			branches = append(branches, name)
		} else {
			reason := "em sincronia — nenhuma ação necessária"
			if left.Branches[name] == "" && right.Branches[name] == "" {
				reason = "não existe em nenhum dos lados"
			} else if left.Branches[name] == "" {
				reason = "ausente em " + sanitizeTerminalText(pair.Left.Label) + " — contraparte não será criada automaticamente"
			} else if right.Branches[name] == "" {
				reason = "ausente em " + sanitizeTerminalText(pair.Right.Label) + " — contraparte não será criada automaticamente"
			}
			if _, err := fmt.Fprintf(g.output, "  %s: %s.\n", sanitizeTerminalText(name), reason); err != nil {
				return "", config.Pair{}, "", err
			}
		}
	}
	if len(branches) == 0 {
		_, err := fmt.Fprintln(g.output, "Nenhuma branch elegível com versões diferentes. Use status para o inventário.")
		return "", config.Pair{}, "", err
	}
	index, err := g.choose("Qual branch? (existe nos dois lados e tem versões diferentes)", branches)
	if err != nil {
		return "", config.Pair{}, "", err
	}
	return service, pair, branches[index], nil
}

func (g *guide) newFlow(resolve bool) error {
	service, pair, branch, err := g.selectBranch()
	if err != nil || branch == "" {
		return err
	}
	if !resolve {
		if code := g.call("sync", "--service", service, "--branch", branch, "--pair", pair.Name()); code != 0 {
			_, err := fmt.Fprintln(g.output, "A simulação não liberou o sync. Se houver conflito, escolha Resolver conflitos no menu.")
			return err
		}
		approved, err := g.approve("Criar as branches e PRs mostradas acima? Ainda haverá confirmação exata do serviço/branch.", "publicar")
		if err != nil || !approved {
			return err
		}
		g.call("sync", "--service", service, "--branch", branch, "--pair", pair.Name(), "--apply")
		return nil
	}
	index, err := g.choose("Onde abrir a PR? Alvo não significa escolher um lado do conflito.", []string{
		pair.Right.Label + " — integrar mudanças de " + pair.Left.Label + " em " + pair.Right.Owner + "/" + service + "/" + branch,
		pair.Left.Label + " — integrar mudanças de " + pair.Right.Label + " em " + pair.Left.Owner + "/" + service + "/" + branch,
	})
	if err != nil {
		return err
	}
	target, source := pair.Right.Label, pair.Left.Label
	if index == 1 {
		target, source = pair.Left.Label, pair.Right.Label
	}
	directory := filepath.Join(g.home, "git-worktrees", service, "resolve-"+strings.ReplaceAll(branch, "/", "-")+"-"+source+"-to-"+target+"-"+time.Now().UTC().Format("20060102T150405.000000000"))
	if g.call("resolve", "prepare", "--service", service, "--branch", branch, "--pair", pair.Name(), "--target", target, "--workspace", directory) != 0 {
		return fmt.Errorf("preparo não concluído; veja o diagnóstico acima, nenhum push foi autorizado")
	}
	return g.workspaceMenu(directory)
}

type workspaceChoice struct {
	directory string
	state     gitinspect.ResolutionState
}

func discoverWorkspaces(home string, cfg config.Config) ([]workspaceChoice, error) {
	var choices []workspaceChoice
	base := filepath.Join(home, "git-worktrees")
	info, err := os.Lstat(base)
	if os.IsNotExist(err) {
		return choices, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return choices, nil
	}
	for _, service := range cfg.Services {
		root := filepath.Join(base, service)
		info, err := os.Lstat(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			directory := filepath.Join(root, entry.Name())
			state, err := gitinspect.LoadResolution(directory)
			if err != nil || validateResolutionScope(cfg, state.Service, state.Branch, state.Target, resolutionSource(state)) != nil || state.Service != service {
				continue
			}
			choices = append(choices, workspaceChoice{directory: directory, state: state})
		}
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].directory > choices[j].directory })
	return choices, nil
}

func (g *guide) selectWorkspace() (string, error) {
	choices, err := discoverWorkspaces(g.home, g.cfg)
	if err != nil {
		return "", err
	}
	var labels []string
	for _, choice := range choices {
		name := filepath.Base(choice.directory)
		prefix := "resolve-" + strings.ReplaceAll(choice.state.Branch, "/", "-") + "-" + resolutionSource(choice.state) + "-to-" + choice.state.Target + "-"
		name = strings.TrimPrefix(name, prefix)
		runes := []rune(name)
		if len(runes) > 27 {
			name = string(runes[:24]) + "..."
		}
		labels = append(labels, choice.state.Service+" / "+choice.state.Branch+" ("+resolutionSource(choice.state)+" → "+choice.state.Target+") — "+name)
	}
	labels = append(labels, "Informar outro caminho (para workspace criado fora do diretório padrão)")
	if len(choices) == 0 {
		if _, err := fmt.Fprintln(g.output, "Nenhuma resolução válida encontrada em ~/git-worktrees. Você pode preparar uma nova no menu anterior."); err != nil {
			return "", err
		}
	}
	index, err := g.choose("Qual resolução deseja retomar?", labels)
	if err != nil {
		return "", err
	}
	if index < len(choices) {
		return choices[index].directory, nil
	}
	directory, err := g.line("Caminho da resolução (Enter cancela): ")
	if err != nil {
		return "", err
	}
	if directory == "" {
		return "", errGuideCancelled
	}
	if strings.HasPrefix(directory, "~/") {
		directory = filepath.Join(g.home, strings.TrimPrefix(directory, "~/"))
	}
	return directory, nil
}

func (g *guide) workspaceMenu(directory string) error {
	state, err := gitinspect.LoadResolution(directory)
	if err != nil {
		return err
	}
	if err := validateResolutionScope(g.cfg, state.Service, state.Branch, state.Target, resolutionSource(state)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(g.output, "Workspace: "+sanitizeTerminalText(directory)+"\nO caminho fica selecionado para todas as próximas ações; não precisa copiá-lo."); err != nil {
		return err
	}
	for {
		index, err := g.choose("Resolução selecionada: "+state.Service+" / "+state.Branch+" ("+resolutionSource(state)+" → "+state.Target+")", []string{
			"Ver conflitos e próximos passos",
			"Ver diferenças para revisão — só leitura",
			"Marcar um arquivo revisado como resolvido (git add)",
			"Rodar uma verificação — escolher comando e autorizar",
			"Ver simulação da PR — nenhum push",
			"Publicar a PR — exige revisão e confirmação digitada",
		})
		if err != nil {
			return err
		}
		switch index {
		case 0:
			g.call("resolve", "status", "--workspace", directory)
		case 1:
			err = g.showDiff(directory)
		case 2:
			err = g.stageFile(directory)
		case 3:
			err = g.runCheck(directory)
		case 4:
			g.call("resolve", "publish", "--workspace", directory)
		case 5:
			approved, approvalErr := g.approve("Você revisou TODO o merge, os conflitos e a cobertura dos testes, inclusive código específico de cada organização?", "revisado")
			if approvalErr != nil {
				err = approvalErr
			} else if approved {
				g.call("resolve", "publish", "--workspace", directory, "--apply", "--reviewed")
			}
		}
		if err != nil && !errors.Is(err, errGuideCancelled) {
			return err
		}
	}
}

func (g *guide) showDiff(directory string) error {
	diff, err := gitinspect.New().ResolutionDiff(g.ctx, directory)
	if err != nil {
		return err
	}
	safe := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return '\ufffd'
		}
		return r
	}, diff)
	_, err = io.WriteString(g.output, safe)
	return err
}

func (g *guide) stageFile(directory string) error {
	i := gitinspect.New()
	paths, err := i.ResolutionUnstaged(g.ctx, directory)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		_, err := fmt.Fprintln(g.output, "Nenhum arquivo pendente de staging. Para editar, abra o workspace: "+sanitizeTerminalText(directory))
		return err
	}
	selected, err := g.choose("Qual arquivo você já editou e revisou? Não escolhemos ours/theirs.", paths)
	if err != nil {
		return err
	}
	approved, err := g.approve("Marcar somente "+sanitizeTerminalText(paths[selected])+" como revisado? Marcadores restantes ainda bloquearão os testes.", "revisado")
	if err != nil || !approved {
		return err
	}
	return i.StageResolutionPath(g.ctx, directory, paths[selected])
}

type checkChoice struct {
	label string
	args  []string
}

func suggestedChecks(directory string) []checkChoice {
	var choices []checkChoice
	for _, preset := range []struct {
		file  string
		label string
		args  []string
	}{
		{"go.mod", "Go: testes — go test ./...", []string{"go", "test", "./..."}},
		{"go.mod", "Go: análise estática — go vet ./...", []string{"go", "vet", "./..."}},
		{"Cargo.toml", "Rust: testes — cargo test --locked", []string{"cargo", "test", "--locked"}},
		{"Cargo.toml", "Rust: lint — cargo clippy --locked --all-targets --all-features -- -D warnings", []string{"cargo", "clippy", "--locked", "--all-targets", "--all-features", "--", "-D", "warnings"}},
	} {
		info, err := os.Lstat(filepath.Join(directory, preset.file))
		if err == nil && info.Mode().IsRegular() {
			choices = append(choices, checkChoice{label: preset.label, args: preset.args})
		}
	}
	return choices
}

func (g *guide) runCheck(directory string) error {
	choices := suggestedChecks(directory)
	var labels []string
	for _, choice := range choices {
		labels = append(labels, choice.label)
	}
	labels = append(labels, "Outro executável/comando (argumentos literais, sem interpretar shell)")
	selected, err := g.choose("Qual verificação? Sugestões não garantem o gate completo do projeto.", labels)
	if err != nil {
		return err
	}
	var command []string
	if selected < len(choices) {
		command = choices[selected].args
	} else {
		name, err := g.line("Executável (ex.: go, cargo, ./scripts/check.sh; Enter cancela): ")
		if err != nil {
			return err
		}
		if name == "" {
			return errGuideCancelled
		}
		command = []string{name}
		for {
			argument, err := g.literalLine("Próximo argumento literal, sem credenciais (Enter conclui): ")
			if err != nil {
				return err
			}
			if argument == "" {
				break
			}
			command = append(command, argument)
		}
	}
	selected, err = g.choose("Tempo máximo para esta verificação", []string{"5 minutos", "20 minutos", "60 minutos"})
	if err != nil {
		return err
	}
	approved, err := g.approve("Rodar o comando escolhido neste workspace? Execução LOCAL, SEM sandbox, com suas permissões de arquivos/rede. Só autorize código confiável.", "executar")
	if err != nil || !approved {
		return err
	}
	args := []string{"resolve", "check", "--workspace", directory, "--timeout", []string{"5m", "20m", "60m"}[selected], "--trusted-local", "--"}
	g.call(append(args, command...)...)
	return nil
}
