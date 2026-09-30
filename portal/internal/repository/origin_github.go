package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var githubPartPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var githubRunPathPattern = regexp.MustCompile(`^[0-9]+$`)

type githubOriginProvider struct {
	command    func(context.Context, string, ...string) ([]byte, error)
	executable string
}

func (provider githubOriginProvider) Resolve(repository string) (*Origin, error) {
	if repository == "" {
		return nil, nil
	}
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || !githubPartPattern.MatchString(owner) || !githubPartPattern.MatchString(name) ||
		owner == "." || owner == ".." || name == "." || name == ".." {
		return nil, errors.New("invalid GitHub repository")
	}
	return &Origin{Provider: "github", Repository: repository, Label: "GitHub",
		URL: "https://github.com/" + repository}, nil
}

func (provider githubOriginProvider) valid(origin *Origin) bool {
	if origin == nil {
		return false
	}
	resolved, err := provider.Resolve(origin.Repository)
	return err == nil && resolved != nil && origin.Provider == resolved.Provider && origin.URL == resolved.URL
}

func (provider githubOriginProvider) Links(origin *Origin, branch, defaultBranch, base, head string, archived bool) OriginLinks {
	if !provider.valid(origin) {
		return OriginLinks{}
	}
	links := OriginLinks{RepositoryURL: origin.URL}
	if archived {
		if base != "" && head != "" {
			links.CompareURL = origin.URL + "/compare/" + url.PathEscape(base) + "..." + url.PathEscape(head)
			links.BranchURL = origin.URL + "/tree/" + url.PathEscape(head)
		}
	} else if branch != "" {
		links.BranchURL = origin.URL + "/tree/" + url.PathEscape(branch)
		if defaultBranch != "" {
			links.CompareURL = origin.URL + "/compare/" + url.PathEscape(defaultBranch) + "..." + url.PathEscape(branch)
		}
	}
	if branch != "" {
		links.WorkflowsURL = origin.URL + "/actions?query=" + url.QueryEscape("branch:"+branch)
	}
	return links
}

func (provider githubOriginProvider) CommitURL(origin *Origin, sha string) string {
	if !provider.valid(origin) || !gitObjectPattern.MatchString(sha) {
		return ""
	}
	return origin.URL + "/commit/" + url.PathEscape(sha)
}

func (provider githubOriginProvider) Repository(ctx context.Context, origin *Origin, branch string) (OriginRepository, error) {
	if origin == nil {
		return OriginRepository{}, errors.New("GitHub repository is not recorded")
	}
	if !provider.valid(origin) {
		return OriginRepository{}, errors.New("invalid GitHub repository")
	}
	owner, name, _ := strings.Cut(origin.Repository, "/")
	query := `query($owner:String!,$name:String!,$qualifiedName:String!){repository(owner:$owner,name:$name){defaultBranchRef{name} ref(qualifiedName:$qualifiedName){target{oid}}}}`
	output, err := provider.command(ctx, provider.executable, "api", "graphql",
		"-f", "query="+query, "-f", "owner="+owner, "-f", "name="+name,
		"-f", "qualifiedName=refs/heads/"+branch)
	if err != nil {
		return OriginRepository{}, commandError(ctx, err, output)
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
		return OriginRepository{}, fmt.Errorf("decode GitHub repository status: %w", err)
	}
	if response.Data.Repository == nil {
		return OriginRepository{}, errors.New("GitHub repository is unavailable")
	}
	result := OriginRepository{}
	if response.Data.Repository.DefaultBranchRef != nil {
		result.DefaultBranch = response.Data.Repository.DefaultBranchRef.Name
	}
	if response.Data.Repository.Ref != nil {
		result.HeadSHA = response.Data.Repository.Ref.Target.OID
		if !gitObjectPattern.MatchString(result.HeadSHA) {
			return OriginRepository{}, errors.New("GitHub returned an invalid branch head")
		}
	}
	return result, nil
}

func (provider githubOriginProvider) CompareHeads(ctx context.Context, origin *Origin, localHead, remoteHead string) (string, error) {
	if !provider.valid(origin) || !gitObjectPattern.MatchString(localHead) || !gitObjectPattern.MatchString(remoteHead) {
		return "", errors.New("invalid GitHub comparison")
	}
	endpoint := fmt.Sprintf("repos/%s/compare/%s...%s", origin.Repository, localHead, remoteHead)
	output, err := provider.command(ctx, provider.executable, "api", endpoint, "--jq", "{status: .status}")
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

func (provider githubOriginProvider) Workflows(ctx context.Context, origin *Origin, branch, exactHead string) ([]Run, error) {
	if origin == nil {
		return nil, errors.New("GitHub repository is not recorded")
	}
	if !provider.valid(origin) {
		return nil, errors.New("invalid GitHub repository")
	}
	args := []string{"run", "list", "-R", origin.Repository, "--branch", branch, "--limit", "100",
		"--json", "workflowName,status,conclusion,headSha,url"}
	if exactHead != "" {
		args = append(args, "--commit", exactHead)
	}
	output, err := provider.command(ctx, provider.executable, args...)
	if err != nil {
		return nil, commandError(ctx, err, output)
	}
	var runs []Run
	if err := json.Unmarshal(output, &runs); err != nil {
		return nil, fmt.Errorf("decode workflow runs: %w", err)
	}
	filtered := make([]Run, 0, len(runs))
	for _, run := range runs {
		if exactHead != "" && run.HeadSHA != exactHead {
			continue
		}
		run.URL = provider.workflowRunURL(origin, run.URL)
		filtered = append(filtered, run)
	}
	return filtered, nil
}

func (provider githubOriginProvider) workflowRunURL(origin *Origin, raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	prefix := "/" + origin.Repository + "/actions/runs/"
	if !strings.HasPrefix(parsed.Path, prefix) || !githubRunPathPattern.MatchString(strings.TrimPrefix(parsed.Path, prefix)) {
		return ""
	}
	return origin.URL + "/actions/runs/" + strings.TrimPrefix(parsed.Path, prefix)
}
