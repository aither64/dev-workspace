package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aither64/vpsfree-cz-workspace/portal/internal/session"
)

const (
	PushStatusExactlyPushed = "exactly-pushed"
	PushStatusNotPushed     = "not-pushed"
	PushStatusRemoteAhead   = "remote-ahead"
	PushStatusDivergent     = "divergent"
	PushStatusUnknown       = "unknown"

	defaultCommandTimeout = 4 * time.Second
)

var gitObjectPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var githubPartPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

type Run struct {
	WorkflowName string `json:"workflowName"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	HeadSHA      string `json:"headSha"`
	URL          string `json:"url"`
}

type Status struct {
	Name          string `json:"name"`
	Project       string `json:"project"`
	GitHub        string `json:"github,omitempty"`
	Branch        string `json:"branch,omitempty"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	HeadSHA       string `json:"headSha,omitempty"`
	LocalHeadSHA  string `json:"localHeadSha,omitempty"`
	RemoteHeadSHA string `json:"remoteHeadSha,omitempty"`
	PushStatus    string `json:"pushStatus,omitempty"`
	StatusError   string `json:"statusError,omitempty"`
	CompareURL    string `json:"compareUrl,omitempty"`
	BranchURL     string `json:"branchUrl,omitempty"`
	ActionsURL    string `json:"actionsUrl,omitempty"`
	GitHubError   string `json:"githubError,omitempty"`
	Runs          []Run  `json:"runs,omitempty"`
	baseSHA       string
	immutable     bool
	worktree      string
}

type Runner struct {
	GH             string
	Workspace      string
	CommandTimeout time.Duration
}

// Inspect derives archived links from immutable manifest records. For active
// sessions, it also resolves the exact canonical worktree and compares its
// checked-out feature head with GitHub's authoritative branch head.
func (r Runner) Inspect(ctx context.Context, slug string, repositories []session.Repository, immutable bool) []Status {
	statuses := make([]Status, 0, len(repositories))
	for _, item := range repositories {
		status := Status{
			Name: item.Name, Project: item.Project, GitHub: item.GitHub, Branch: item.Branch,
			DefaultBranch: item.DefaultBranch, HeadSHA: item.FinalHeadSHA,
			baseSHA: item.InitialBaseSHA, immutable: immutable,
		}
		if !immutable {
			status.PushStatus = PushStatusUnknown
			if workspace, err := canonicalPath(r.Workspace); err != nil {
				status.StatusError = fmt.Sprintf("resolve workspace: %v", err)
			} else if !session.ValidSlug(slug) || !session.ValidSlug(item.Name) {
				status.StatusError = "resolve worktree: invalid session or repository name"
			} else {
				status.worktree = filepath.Join(workspace, "worktrees", slug, item.Name)
			}
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
		if statuses[index].immutable && statuses[index].GitHub == "" {
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
				status.recordContextError(ctx.Err())
			}
		}(&statuses[index])
	}
	group.Wait()
}

func (r Runner) Enrich(ctx context.Context, status *Status) {
	timeout := r.CommandTimeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if status.immutable {
		r.enrichArchived(ctx, status)
		return
	}
	r.enrichActive(ctx, status)
}

type localResult struct {
	head     string
	worktree string
	err      error
}

type githubResult struct {
	defaultBranch string
	remoteHead    string
	err           error
}

func (r Runner) enrichActive(ctx context.Context, status *Status) {
	localChannel := make(chan localResult, 1)
	go func() {
		head, err := r.resolveLocalHead(ctx, status)
		localChannel <- localResult{head: head, worktree: status.worktree, err: err}
	}()

	githubChannel := make(chan githubResult, 1)
	if status.GitHub == "" {
		githubChannel <- githubResult{err: errors.New("GitHub repository is not recorded")}
	} else {
		go func() { githubChannel <- r.githubRepository(ctx, status) }()
	}

	local := <-localChannel
	remote := <-githubChannel
	if local.err != nil {
		status.StatusError = conciseError(local.err, nil)
	} else {
		status.LocalHeadSHA = local.head
	}
	if remote.err != nil {
		status.GitHubError = conciseError(remote.err, nil)
	} else {
		if remote.defaultBranch != "" {
			status.DefaultBranch = remote.defaultBranch
		}
		status.RemoteHeadSHA = remote.remoteHead
	}
	status.updateLinks()

	if local.err != nil || remote.err != nil {
		return
	}
	if status.RemoteHeadSHA == "" {
		status.PushStatus = PushStatusNotPushed
		return
	}
	if status.LocalHeadSHA == status.RemoteHeadSHA {
		status.PushStatus = PushStatusExactlyPushed
		r.loadRuns(ctx, status, status.LocalHeadSHA)
		return
	}

	pushStatus, err := r.compareHeads(ctx, local.worktree, status.LocalHeadSHA, status.RemoteHeadSHA)
	if err != nil {
		pushStatus, err = r.compareHeadsOnGitHub(ctx, status)
	}
	if err != nil {
		status.StatusError = "compare local and GitHub heads: " + conciseError(err, nil)
		return
	}
	status.PushStatus = pushStatus
}

func (r Runner) enrichArchived(ctx context.Context, status *Status) {
	if status.Branch == "" || status.HeadSHA == "" {
		return
	}
	r.loadRuns(ctx, status, status.HeadSHA)
}

func (r Runner) resolveLocalHead(ctx context.Context, status *Status) (string, error) {
	if status.worktree == "" {
		if status.StatusError != "" {
			return "", errors.New(status.StatusError)
		}
		return "", errors.New("canonical worktree is not configured")
	}
	if status.Branch == "" {
		return "", errors.New("feature branch is not recorded")
	}

	info, err := os.Lstat(status.worktree)
	if err != nil {
		return "", fmt.Errorf("inspect canonical worktree: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("canonical worktree is not a real directory")
	}
	realWorktree, err := canonicalPath(status.worktree)
	if err != nil {
		return "", fmt.Errorf("resolve canonical worktree: %w", err)
	}
	if realWorktree != filepath.Clean(status.worktree) {
		return "", errors.New("canonical worktree path contains a symbolic link")
	}

	output, err := r.command(ctx, "git", "-C", realWorktree, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("inspect canonical worktree identity: %w", commandError(ctx, err, output))
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 2 {
		return "", errors.New("inspect canonical worktree identity: unexpected git output")
	}
	topLevel, err := canonicalPath(lines[0])
	if err != nil || topLevel != realWorktree {
		return "", errors.New("canonical worktree has an unexpected top level")
	}
	commonDir, err := canonicalPath(lines[1])
	if err != nil {
		return "", fmt.Errorf("resolve worktree common directory: %w", err)
	}
	expectedCommon, err := r.expectedCommonDir(status.Project)
	if err != nil {
		return "", err
	}
	if commonDir != expectedCommon {
		return "", errors.New("canonical worktree belongs to an unexpected repository")
	}

	ref := "refs/heads/" + status.Branch
	output, err = r.command(
		ctx, "git", "-C", realWorktree, "for-each-ref",
		"--format=%(refname)%00%(objectname)%00%(HEAD)", "--count=1", "--", ref,
	)
	if err != nil {
		return "", fmt.Errorf("resolve local feature head: %w", commandError(ctx, err, output))
	}
	fields := strings.Split(strings.TrimSuffix(string(output), "\n"), "\x00")
	if len(fields) != 3 || fields[0] != ref || fields[2] != "*" || !gitObjectPattern.MatchString(fields[1]) {
		return "", errors.New("canonical worktree is not on its recorded feature branch")
	}
	return fields[1], nil
}

func (r Runner) expectedCommonDir(project string) (string, error) {
	workspace, err := canonicalPath(r.Workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	if !session.ValidSlug(project) {
		return "", errors.New("repository project is invalid")
	}
	path := filepath.Join(workspace, "repos", project+".git")
	if project == "workspace" {
		path = filepath.Join(workspace, ".git")
	}
	resolved, err := canonicalPath(path)
	if err != nil {
		return "", fmt.Errorf("resolve canonical repository: %w", err)
	}
	return resolved, nil
}

func (r Runner) githubRepository(ctx context.Context, status *Status) githubResult {
	owner, name, ok := strings.Cut(status.GitHub, "/")
	if !ok || !githubPartPattern.MatchString(owner) || !githubPartPattern.MatchString(name) {
		return githubResult{err: errors.New("invalid GitHub repository")}
	}
	query := `query($owner:String!,$name:String!,$qualifiedName:String!){repository(owner:$owner,name:$name){defaultBranchRef{name} ref(qualifiedName:$qualifiedName){target{oid}}}}`
	output, err := r.command(
		ctx, r.gh(), "api", "graphql",
		"-f", "query="+query,
		"-f", "owner="+owner,
		"-f", "name="+name,
		"-f", "qualifiedName=refs/heads/"+status.Branch,
	)
	if err != nil {
		return githubResult{err: commandError(ctx, err, output)}
	}
	var response struct {
		Data struct {
			Repository *struct {
				DefaultBranchRef *struct {
					Name string `json:"name"`
				} `json:"defaultBranchRef"`
				Ref *struct {
					Target struct {
						OID string `json:"oid"`
					} `json:"target"`
				} `json:"ref"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return githubResult{err: fmt.Errorf("decode GitHub repository status: %w", err)}
	}
	if response.Data.Repository == nil {
		return githubResult{err: errors.New("GitHub repository is unavailable")}
	}
	result := githubResult{}
	if response.Data.Repository.DefaultBranchRef != nil {
		result.defaultBranch = response.Data.Repository.DefaultBranchRef.Name
	}
	if response.Data.Repository.Ref != nil {
		result.remoteHead = response.Data.Repository.Ref.Target.OID
		if !gitObjectPattern.MatchString(result.remoteHead) {
			return githubResult{err: errors.New("GitHub returned an invalid branch head")}
		}
	}
	return result
}

func (r Runner) compareHeads(ctx context.Context, worktree, localHead, remoteHead string) (string, error) {
	ancestor, err := r.isAncestor(ctx, worktree, remoteHead, localHead)
	if err != nil {
		return "", err
	}
	if ancestor {
		return PushStatusNotPushed, nil
	}
	ancestor, err = r.isAncestor(ctx, worktree, localHead, remoteHead)
	if err != nil {
		return "", err
	}
	if ancestor {
		return PushStatusRemoteAhead, nil
	}
	return PushStatusDivergent, nil
}

func (r Runner) isAncestor(ctx context.Context, worktree, ancestor, descendant string) (bool, error) {
	output, err := r.command(ctx, "git", "-C", worktree, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return false, nil
	}
	return false, commandError(ctx, err, output)
}

func (r Runner) compareHeadsOnGitHub(ctx context.Context, status *Status) (string, error) {
	endpoint := fmt.Sprintf(
		"repos/%s/compare/%s...%s", status.GitHub, status.LocalHeadSHA, status.RemoteHeadSHA,
	)
	output, err := r.command(ctx, r.gh(), "api", endpoint, "--jq", "{status: .status}")
	if err != nil {
		return "", commandError(ctx, err, output)
	}
	var response struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return "", fmt.Errorf("decode GitHub comparison: %w", err)
	}
	switch response.Status {
	case "ahead":
		return PushStatusRemoteAhead, nil
	case "behind":
		return PushStatusNotPushed, nil
	case "diverged":
		return PushStatusDivergent, nil
	default:
		return "", fmt.Errorf("GitHub returned comparison status %q", response.Status)
	}
}

func (r Runner) loadRuns(ctx context.Context, status *Status, exactHead string) {
	args := []string{
		"run", "list", "-R", status.GitHub, "--branch", status.Branch, "--limit", "10",
		"--json", "workflowName,status,conclusion,headSha,url",
	}
	if exactHead != "" {
		args = append(args, "--commit", exactHead)
	}
	output, err := r.command(ctx, r.gh(), args...)
	if err != nil {
		status.GitHubError = conciseError(commandError(ctx, err, output), nil)
		return
	}
	var runs []Run
	if err := json.Unmarshal(output, &runs); err != nil {
		status.GitHubError = fmt.Sprintf("decode workflow runs: %v", err)
		return
	}
	if exactHead == "" {
		status.Runs = runs
		return
	}
	for _, run := range runs {
		if run.HeadSHA == exactHead {
			status.Runs = append(status.Runs, run)
		}
	}
}

func (r Runner) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (r Runner) gh() string {
	if r.GH != "" {
		return r.GH
	}
	return "gh"
}

func (status *Status) recordContextError(err error) {
	if status.immutable {
		status.GitHubError = err.Error()
	} else {
		status.StatusError = err.Error()
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

func canonicalPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func commandError(ctx context.Context, err error, output []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		return err
	}
	return errors.New(message)
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
