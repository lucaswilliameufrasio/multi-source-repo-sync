package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/workflow"
)

// runPropagate puts one branch of one organization into the other copies of the service and opens a
// pull request into each base branch. It is a dry run unless --apply is given and confirmed.
func runPropagate(parent context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("propagate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to repository configuration")
	service := flags.String("service", "", "repository service name (required)")
	branch := flags.String("branch", "", "branch to put into the other copies (required; not a sync branch)")
	from := flags.String("from", "", "label of the organization that has the branch (required)")
	to := flags.String("to", "", "labels of the organizations to put it into, comma separated (default: every other copy)")
	base := flags.String("base", "", "branch the pull requests go into (default: the first sync branch)")
	apply := flags.Bool("apply", false, "create the branches and pull requests (requires typed confirmation)")
	timeout := flags.Duration("timeout", 5*time.Minute, "overall timeout")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *service == "" || *branch == "" || *from == "" {
		_, _ = fmt.Fprintln(stderr, "propagate requires --service, --branch and --from")
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "configuration error: %v\n", err)
		return 2
	}
	var targets []string
	for _, label := range strings.Split(*to, ",") {
		if label = strings.TrimSpace(label); label != "" {
			targets = append(targets, label)
		}
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()

	client := github.NewClient(github.ExecRunner{})
	inspector := gitinspect.New()
	flow := workflow.Workflow{Reader: client, Inspector: inspector, Git: inspector, PRs: client, Merge: client, Branches: inspector}
	proposal, err := flow.ProposePropagation(ctx, cfg, *service, *branch, *from, targets, *base)
	if err != nil {
		return reportWorkflowError(stderr, err)
	}
	printPropagation(stdout, proposal)
	if !*apply {
		_, _ = fmt.Fprintln(stdout, "Dry run only. No branch or pull request was created.")
		return 0
	}
	if !anyWritable(proposal) {
		_, _ = fmt.Fprintln(stderr, "Nothing can be created in any copy.")
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "Type %q to create the listed branches and pull requests: ", proposal.Phrase)
	line, err := bufferedInput(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		_, _ = fmt.Fprintf(stderr, "read confirmation: %v\n", err)
		return 1
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line != proposal.Phrase {
		_, _ = fmt.Fprintln(stderr, "Cancelled; nothing was created.")
		return 0
	}
	results, err := flow.ApplyPropagation(ctx, cfg, workflow.PropagateRequest{
		Service: proposal.Service, Branch: proposal.Branch, Source: proposal.Source, Targets: targetLabels(proposal),
		Base: proposal.Base, Confirmation: line, SourceSHA: proposal.SourceSHA,
	})
	if err != nil {
		return reportWorkflowError(stderr, err)
	}
	failed := false
	for _, result := range results {
		_, _ = fmt.Fprintf(stdout, "%s (%s): %s", result.Label, result.Repository, result.Status)
		if result.PullRequest != "" {
			_, _ = fmt.Fprintf(stdout, "; PR %s (created=%t)", result.PullRequest, result.PRCreated)
		}
		if result.Detail != "" {
			_, _ = fmt.Fprintf(stdout, " — %s", result.Detail)
		}
		_, _ = fmt.Fprintln(stdout)
		failed = failed || result.Status == "failed"
	}
	if failed {
		return 1
	}
	return 0
}

func reportWorkflowError(stderr io.Writer, err error) int {
	if problem, ok := workflow.IsProblem(err); ok {
		_, _ = fmt.Fprintln(stderr, problem.Message)
		return 2
	}
	_, _ = fmt.Fprintf(stderr, "failed: %v\n", err)
	return 1
}

func targetLabels(proposal workflow.PropagateProposal) []string {
	labels := make([]string, 0, len(proposal.Targets))
	for _, target := range proposal.Targets {
		labels = append(labels, target.Label)
	}
	return labels
}

func anyWritable(proposal workflow.PropagateProposal) bool {
	for _, target := range proposal.Targets {
		if target.State == workflow.StateNew || target.State == workflow.StateAlreadyThere {
			return true
		}
	}
	return false
}

var stateText = map[string]string{
	workflow.StateNew:              "would create the branch and open a pull request",
	workflow.StateAlreadyThere:     "has the same commit already; would only open the pull request",
	workflow.StateDifferentHistory: "has a branch of that name with other commits; left alone",
	workflow.StateNoBase:           "has no base branch to propose into; skipped",
	workflow.StateMergeCommitsOff:  "does not allow merge commits; skipped",
}

func printPropagation(writer io.Writer, proposal workflow.PropagateProposal) {
	_, _ = fmt.Fprintf(writer, "%s: branch %s of %s at %s, pull requests into %s\n", proposal.Service, proposal.Branch, proposal.Source, shortSHA(proposal.SourceSHA), proposal.Base)
	for _, target := range proposal.Targets {
		_, _ = fmt.Fprintf(writer, "  %-10s %s %s\n", target.Label, target.Repository, stateText[target.State])
		if target.OpenPullURL != "" {
			_, _ = fmt.Fprintf(writer, "             an open pull request already exists: %s\n", target.OpenPullURL)
		}
	}
}
