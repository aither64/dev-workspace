package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
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

type Run struct {
	WorkflowName string `json:"workflowName"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	HeadSHA      string `json:"headSha"`
	URL          string `json:"url"`
}

type Status struct {
	ReviewID      string       `json:"reviewId"`
	Name          string       `json:"name"`
	Project       string       `json:"project"`
	GitHub        string       `json:"github,omitempty"`
	Origin        *Origin      `json:"origin,omitempty"`
	OriginLinks   *OriginLinks `json:"originLinks,omitempty"`
	OriginError   string       `json:"originError,omitempty"`
	Branch        string       `json:"branch,omitempty"`
	DefaultBranch string       `json:"defaultBranch,omitempty"`
	HeadSHA       string       `json:"headSha,omitempty"`
	LocalHeadSHA  string       `json:"localHeadSha,omitempty"`
	RemoteHeadSHA string       `json:"remoteHeadSha,omitempty"`
	PushStatus    string       `json:"pushStatus,omitempty"`
	StatusError   string       `json:"statusError,omitempty"`
	CompareURL    string       `json:"compareUrl,omitempty"`
	BranchURL     string       `json:"branchUrl,omitempty"`
	ActionsURL    string       `json:"actionsUrl,omitempty"`
	GitHubError   string       `json:"githubError,omitempty"`
	Runs          []Run        `json:"runs,omitempty"`
	baseSHA       string
	immutable     bool
	worktree      string
}

type Runner struct {
	GH             string
	Workspace      string
	CommandTimeout time.Duration
	Provider       OriginProvider
}

// Inspect derives archived links from immutable manifest records. For active
// sessions, it also resolves the exact canonical worktree and compares its
// checked-out feature head with GitHub's authoritative branch head.
func (r Runner) Skeleton(slug string, repositories []session.Repository, immutable bool) []Status {
	statuses := make([]Status, 0, len(repositories))
	provider := r.originProvider()
	for _, item := range repositories {
		status := Status{
			ReviewID: ReviewID(item.Name), Name: item.Name, Project: item.Project, GitHub: item.GitHub, Branch: item.Branch,
			DefaultBranch: item.DefaultBranch, HeadSHA: item.FinalHeadSHA,
			baseSHA: item.InitialBaseSHA, immutable: immutable,
		}
		status.resolveOrigin(provider)
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
		status.updateLinks(provider)
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses
}

func (r Runner) Inspect(ctx context.Context, slug string, repositories []session.Repository, immutable bool) []Status {
	statuses := r.Skeleton(slug, repositories, immutable)
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
	provider := r.originProvider()
	status.resolveOrigin(provider)
	status.updateLinks(provider)

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
type originResult struct {
	result OriginRepository
	err    error
}

func (r Runner) enrichActive(ctx context.Context, status *Status) {
	provider := r.originProvider()
	localChannel := make(chan localResult, 1)
	go func() {
		head, err := r.resolveLocalHead(ctx, status)
		localChannel <- localResult{head: head, worktree: status.worktree, err: err}
	}()

	originChannel := make(chan originResult, 1)
	if status.Origin == nil && status.OriginError != "" {
		originChannel <- originResult{err: errors.New(status.OriginError)}
	} else {
		go func() {
			result, err := provider.Repository(ctx, status.Origin, status.Branch)
			originChannel <- originResult{result: result, err: err}
		}()
	}

	local := <-localChannel
	remote := <-originChannel
	if local.err != nil {
		status.StatusError = conciseError(local.err, nil)
	} else {
		status.LocalHeadSHA = local.head
	}
	if remote.err != nil {
		status.setOriginError(conciseError(remote.err, nil))
	} else {
		status.setOriginError("")
		if remote.result.DefaultBranch != "" {
			status.DefaultBranch = remote.result.DefaultBranch
		}
		status.RemoteHeadSHA = remote.result.HeadSHA
	}
	status.updateLinks(provider)

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
		pushStatus, err = provider.CompareHeads(ctx, status.Origin, status.LocalHeadSHA, status.RemoteHeadSHA)
	}
	if err != nil {
		status.StatusError = "compare local and " + status.Origin.Label + " heads: " + conciseError(err, nil)
		return
	}
	status.PushStatus = pushStatus
}

func (r Runner) enrichArchived(ctx context.Context, status *Status) {
	if status.Origin == nil || status.Branch == "" || status.HeadSHA == "" {
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

func (r Runner) loadRuns(ctx context.Context, status *Status, exactHead string) {
	runs, err := r.originProvider().Workflows(ctx, status.Origin, status.Branch, exactHead)
	if err != nil {
		status.setOriginError(conciseError(err, nil))
		return
	}
	status.setOriginError("")
	status.Runs = runs
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
		status.setOriginError(err.Error())
	} else {
		status.StatusError = err.Error()
	}
}

func (status *Status) resolveOrigin(provider OriginProvider) {
	origin, err := provider.Resolve(status.GitHub)
	status.Origin = origin
	if err != nil {
		status.setOriginError(err.Error())
	}
}

func (status *Status) setOriginError(message string) {
	status.OriginError = message
	status.GitHubError = message // Existing JSON clients use githubError.
}

func (status *Status) updateLinks(provider OriginProvider) {
	status.CompareURL = ""
	status.BranchURL = ""
	status.ActionsURL = ""
	status.OriginLinks = nil
	if status.Origin == nil {
		return
	}
	links := provider.Links(status.Origin, status.Branch, status.DefaultBranch,
		status.baseSHA, status.HeadSHA, status.immutable)
	status.OriginLinks = &links
	status.CompareURL = links.CompareURL
	status.BranchURL = links.BranchURL
	status.ActionsURL = links.WorkflowsURL
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
