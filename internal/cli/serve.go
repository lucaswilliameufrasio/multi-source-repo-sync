package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/web"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/workflow"
)

// runServe opens the local web interface. It prints the address, which carries the access token.
func runServe(parent context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to repository configuration")
	address := flags.String("addr", "127.0.0.1:7878", "loopback address to listen on (host:port; port 0 picks a free one)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "unexpected positional arguments: %v\n", flags.Args())
		return 2
	}
	if err := web.RequireLoopback(*address); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "configuration error: %v\n", err)
		return 2
	}
	token, err := web.NewToken()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "could not make an access token: %v\n", err)
		return 1
	}

	client := github.NewClient(github.ExecRunner{})
	inspector := gitinspect.New()
	flow := workflow.Workflow{Reader: client, Inspector: inspector, Git: inspector, PRs: client, Merge: client, Branches: inspector}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "could not listen on %s: %v\n", *address, err)
		return 1
	}
	server := &http.Server{
		Handler:           web.New(cfg, flow, token).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()

	_, _ = fmt.Fprintf(stdout, "Interface local em http://%s/?token=%s\nAbra esse endereço no navegador desta máquina; ele só vale enquanto este comando estiver rodando. Ctrl+C encerra.\n", listener.Addr(), token)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		_, _ = fmt.Fprintf(stderr, "server stopped: %v\n", err)
		return 1
	}
	return 0
}
