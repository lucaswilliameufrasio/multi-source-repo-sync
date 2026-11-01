package domain

type Report struct {
	Mode         string             `json:"mode"`
	Repositories []RepositoryStatus `json:"repositories"`
}

// RepositoryStatus is one service in one pair of organizations.
type RepositoryStatus struct {
	Service  string         `json:"service"`
	Pair     string         `json:"pair"`
	Left     RepositorySide `json:"left"`
	Right    RepositorySide `json:"right"`
	Branches []BranchStatus `json:"branches"`
	Errors   []string       `json:"errors,omitempty"`
}

type RepositorySide struct {
	Label            string        `json:"label"`
	Owner            string        `json:"owner"`
	Repository       string        `json:"repository"`
	Branches         []BranchRef   `json:"branches"`
	OpenPullRequests []PullRequest `json:"open_pull_requests"`
	Error            string        `json:"error,omitempty"`
}

type BranchRef struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

type PullRequest struct {
	Side       string `json:"side"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
	HeadBranch string `json:"head_branch"`
	BaseBranch string `json:"base_branch"`
	Draft      bool   `json:"draft"`
	URL        string `json:"url"`
}

type BranchStatus struct {
	Name       string `json:"name"`
	Policy     string `json:"policy"`
	LeftSHA    string `json:"left_sha,omitempty"`
	RightSHA   string `json:"right_sha,omitempty"`
	Assessment string `json:"assessment"`
	Action     string `json:"action"`
	// PRTargets are labels of the organizations that receive a pull request.
	PRTargets []string `json:"pr_targets,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}
