package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/domain"
)

type Snapshot struct {
	Branches         map[string]string
	OpenPullRequests []domain.PullRequest
}

type PullRequestResult struct {
	Number  int
	URL     string
	Created bool
}

type RepositoryReader interface {
	ReadRepository(ctx context.Context, owner string, repository string, side string) (Snapshot, error)
}

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.Output()
	if err == nil {
		return output, nil
	}

	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return nil, fmt.Errorf("command failed with exit code %d", exitError.ExitCode())
	}
	return nil, fmt.Errorf("run command: %w", err)
}

type Client struct {
	runner CommandRunner
}

func NewClient(runner CommandRunner) Client {
	return Client{runner: runner}
}

func (c Client) ReadRepository(ctx context.Context, owner string, repository string, side string) (Snapshot, error) {
	branches, err := c.readBranches(ctx, owner, repository)
	if err != nil {
		return Snapshot{}, err
	}

	pullRequests, err := c.readOpenPullRequests(ctx, owner, repository, side)
	if err != nil {
		return Snapshot{}, err
	}

	return Snapshot{
		Branches:         branches,
		OpenPullRequests: pullRequests,
	}, nil
}

func (c Client) EnsurePullRequest(
	ctx context.Context,
	owner string,
	repository string,
	headBranch string,
	baseBranch string,
	title string,
	body string,
) (PullRequestResult, error) {
	existing, err := c.findOpenPullRequest(ctx, owner, repository, headBranch, baseBranch)
	if err != nil {
		return PullRequestResult{}, err
	}
	if existing != nil {
		return PullRequestResult{Number: existing.Number, URL: existing.URL}, nil
	}

	path := fmt.Sprintf("repos/%s/%s/pulls", owner, repository)
	output, err := c.runner.Run(
		ctx,
		"gh",
		"api",
		"--method",
		"POST",
		path,
		"-f",
		"title="+title,
		"-f",
		"head="+headBranch,
		"-f",
		"base="+baseBranch,
		"-f",
		"body="+body,
	)
	if err != nil {
		return PullRequestResult{}, fmt.Errorf("create pull request: %w", err)
	}

	var pullRequest struct {
		Number int    `json:"number"`
		URL    string `json:"html_url"`
	}
	if err := json.Unmarshal(output, &pullRequest); err != nil {
		return PullRequestResult{}, fmt.Errorf("decode created pull request: %w", err)
	}
	if pullRequest.Number < 1 || pullRequest.URL == "" {
		return PullRequestResult{}, fmt.Errorf("created pull request response is missing its number or URL")
	}
	return PullRequestResult{Number: pullRequest.Number, URL: pullRequest.URL, Created: true}, nil
}

func (c Client) MergeCommitAllowed(ctx context.Context, owner string, repository string) (bool, error) {
	path := fmt.Sprintf("repos/%s/%s", owner, repository)
	output, err := c.runner.Run(ctx, "gh", "api", "--jq", ".allow_merge_commit", path)
	if err != nil {
		return false, fmt.Errorf("read merge-commit setting: %w", err)
	}
	allowed, err := strconv.ParseBool(strings.TrimSpace(string(output)))
	if err != nil {
		return false, fmt.Errorf("decode merge-commit setting: %w", err)
	}
	return allowed, nil
}

func (c Client) findOpenPullRequest(ctx context.Context, owner string, repository string, headBranch string, baseBranch string) (*PullRequestResult, error) {
	query := url.Values{}
	query.Set("state", "open")
	query.Set("head", owner+":"+headBranch)
	query.Set("base", baseBranch)
	query.Set("per_page", "100")
	path := fmt.Sprintf("repos/%s/%s/pulls?%s", owner, repository, query.Encode())
	output, err := c.runner.Run(ctx, "gh", "api", "--paginate", "--slurp", path)
	if err != nil {
		return nil, fmt.Errorf("find existing pull request: %w", err)
	}

	var pages [][]struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := json.Unmarshal(output, &pages); err != nil {
		return nil, fmt.Errorf("decode existing pull requests: %w", err)
	}

	for _, page := range pages {
		for _, pullRequest := range page {
			if pullRequest.Head.Ref == headBranch && pullRequest.Base.Ref == baseBranch {
				return &PullRequestResult{Number: pullRequest.Number, URL: pullRequest.HTMLURL}, nil
			}
		}
	}
	return nil, nil
}

func (c Client) readBranches(ctx context.Context, owner string, repository string) (map[string]string, error) {
	path := fmt.Sprintf("repos/%s/%s/branches?per_page=100", owner, repository)
	output, err := c.runner.Run(ctx, "gh", "api", "--paginate", "--slurp", path)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}

	var pages [][]struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(output, &pages); err != nil {
		return nil, fmt.Errorf("decode branch response: %w", err)
	}

	branches := make(map[string]string)
	for _, page := range pages {
		for _, branch := range page {
			if branch.Name == "" || branch.Commit.SHA == "" {
				return nil, fmt.Errorf("decode branch response: branch name or commit SHA is empty")
			}
			branches[branch.Name] = branch.Commit.SHA
		}
	}

	return branches, nil
}

func (c Client) readOpenPullRequests(ctx context.Context, owner string, repository string, side string) ([]domain.PullRequest, error) {
	path := fmt.Sprintf("repos/%s/%s/pulls?state=open&per_page=100", owner, repository)
	output, err := c.runner.Run(ctx, "gh", "api", "--paginate", "--slurp", path)
	if err != nil {
		return nil, fmt.Errorf("list open pull requests: %w", err)
	}

	var pages [][]struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Draft   bool   `json:"draft"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := json.Unmarshal(output, &pages); err != nil {
		return nil, fmt.Errorf("decode pull request response: %w", err)
	}

	var pullRequests []domain.PullRequest
	for _, page := range pages {
		for _, pullRequest := range page {
			if pullRequest.Number < 1 || pullRequest.Head.Ref == "" || pullRequest.Base.Ref == "" {
				return nil, fmt.Errorf("decode pull request response: required pull request fields are empty")
			}
			pullRequests = append(pullRequests, domain.PullRequest{
				Side:       side,
				Number:     pullRequest.Number,
				Title:      pullRequest.Title,
				HeadBranch: pullRequest.Head.Ref,
				BaseBranch: pullRequest.Base.Ref,
				Draft:      pullRequest.Draft,
				URL:        pullRequest.HTMLURL,
			})
		}
	}

	return pullRequests, nil
}

func RepositoryURL(owner string, repository string) string {
	return "https://github.com/" + strings.Trim(owner, "/") + "/" + strings.Trim(repository, "/") + ".git"
}
