package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/aither64/vpsfree-cz-workspace/portal/internal/session"
)

type Run struct {
	WorkflowName string `json:"workflowName"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	HeadSHA      string `json:"headSha"`
	URL          string `json:"url"`
}

type Status struct {
	Name          string `json:"name"`
	GitHub        string `json:"github,omitempty"`
	Branch        string `json:"branch,omitempty"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	HeadSHA       string `json:"headSha,omitempty"`
	CompareURL    string `json:"compareUrl,omitempty"`
	BranchURL     string `json:"branchUrl,omitempty"`
	ActionsURL    string `json:"actionsUrl,omitempty"`
	GitHubError   string `json:"githubError,omitempty"`
	Runs          []Run  `json:"runs,omitempty"`
	baseSHA       string
	immutable     bool
}

type Runner struct {
	GH string
}

// Inspect derives repository links only from the validated portal manifest.
// Worktrees are writable from the development LXC, so the host portal must not
// invoke Git or trust repository configuration while serving a page.
func (r Runner) Inspect(ctx context.Context, repositories []session.Repository, closed bool) []Status {
	statuses := make([]Status, 0, len(repositories))
	for _, item := range repositories {
		status := Status{
			Name: item.Name, GitHub: item.GitHub, Branch: item.Branch,
			DefaultBranch: item.DefaultBranch, HeadSHA: item.FinalHeadSHA,
			baseSHA: item.InitialBaseSHA, immutable: closed,
		}
		status.updateLinks()
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	r.EnrichAll(ctx, statuses)
	return statuses
}

func (r Runner) EnrichAll(ctx context.Context, statuses []Status) {
	var group sync.WaitGroup
	semaphore := make(chan struct{}, 4)
	for index := range statuses {
		if statuses[index].GitHub == "" {
			continue
		}
		group.Add(1)
		go func(status *Status) {
			defer group.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
				r.Enrich(ctx, status)
			case <-ctx.Done():
				status.GitHubError = ctx.Err().Error()
			}
		}(&statuses[index])
	}
	group.Wait()
}

func (r Runner) Enrich(ctx context.Context, status *Status) {
	gh := r.GH
	if gh == "" {
		gh = "gh"
	}
	type commandResult struct {
		output []byte
		err    error
	}
	var defaultBranchResult <-chan commandResult
	if !status.immutable {
		channel := make(chan commandResult, 1)
		defaultBranchResult = channel
		go func() {
			output, err := exec.CommandContext(ctx, gh, "repo", "view", status.GitHub, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name").CombinedOutput()
			channel <- commandResult{output: output, err: err}
		}()
	}
	var runsResult <-chan commandResult
	if status.Branch != "" {
		channel := make(chan commandResult, 1)
		runsResult = channel
		go func() {
			output, err := exec.CommandContext(ctx, gh, "run", "list", "-R", status.GitHub, "--branch", status.Branch, "--limit", "10", "--json", "workflowName,status,conclusion,headSha,url").CombinedOutput()
			channel <- commandResult{output: output, err: err}
		}()
	}
	if defaultBranchResult != nil {
		result := <-defaultBranchResult
		if result.err == nil {
			status.DefaultBranch = strings.TrimSpace(string(result.output))
		} else {
			status.GitHubError = conciseError(result.err, result.output)
			status.DefaultBranch = ""
		}
		status.updateLinks()
	}
	if runsResult == nil {
		return
	}
	result := <-runsResult
	if result.err != nil {
		if status.GitHubError == "" {
			status.GitHubError = conciseError(result.err, result.output)
		}
		return
	}
	if err := json.Unmarshal(result.output, &status.Runs); err != nil {
		status.GitHubError = fmt.Sprintf("decode workflow runs: %v", err)
	}
}

func (status *Status) updateLinks() {
	if status.GitHub == "" {
		return
	}
	base := "https://github.com/" + status.GitHub
	if status.immutable {
		status.CompareURL = ""
		status.BranchURL = ""
		if status.baseSHA != "" && status.HeadSHA != "" {
			status.CompareURL = base + "/compare/" + url.PathEscape(status.baseSHA) + "..." + url.PathEscape(status.HeadSHA)
			status.BranchURL = base + "/tree/" + url.PathEscape(status.HeadSHA)
		}
	} else {
		status.CompareURL = ""
		status.BranchURL = ""
		if status.Branch != "" {
			status.BranchURL = base + "/tree/" + url.PathEscape(status.Branch)
			if status.DefaultBranch != "" {
				status.CompareURL = base + "/compare/" + url.PathEscape(status.DefaultBranch) + "..." + url.PathEscape(status.Branch)
			}
		}
	}
	if status.Branch != "" {
		status.ActionsURL = base + "/actions?query=" + url.QueryEscape("branch:"+status.Branch)
	}
}

func conciseError(err error, output []byte) string {
	message := strings.TrimSpace(string(output))
	if message == "" {
		message = err.Error()
	}
	if len(message) > 240 {
		message = message[:240] + "…"
	}
	return message
}
