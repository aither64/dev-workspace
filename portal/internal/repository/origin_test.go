package repository

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestGitHubOriginValidatesIdentityAndProducesEscapedLinks(t *testing.T) {
	provider := githubOriginProvider{}
	origin, err := provider.Resolve("example/project")
	if err != nil || origin.Provider != "github" || origin.Repository != "example/project" ||
		origin.Label != "GitHub" || origin.URL != "https://github.com/example/project" {
		t.Fatalf("resolved origin = %#v, %v", origin, err)
	}
	for _, value := range []string{"https://other.test/repo", "example/project/extra", "../project", "example/..", "example/project?x=1"} {
		if resolved, err := provider.Resolve(value); err == nil || resolved != nil {
			t.Fatalf("accepted origin %q: %#v, %v", value, resolved, err)
		}
	}
	forged := *origin
	forged.URL = "https://other.test/repo"
	if provider.CommitURL(&forged, strings.Repeat("a", 40)) != "" ||
		provider.Links(&forged, "feature", "main", "", "", false).RepositoryURL != "" {
		t.Fatal("forged origin produced an external link")
	}
	branch, base := "feature/topic", "main & old"
	links := provider.Links(origin, branch, base, "", "", false)
	wantBranch := origin.URL + "/tree/" + url.PathEscape(branch)
	wantCompare := origin.URL + "/compare/" + url.PathEscape(base) + "..." + url.PathEscape(branch)
	if links.RepositoryURL != origin.URL || links.BranchURL != wantBranch ||
		links.CompareURL != wantCompare ||
		links.WorkflowsURL != origin.URL+"/actions?query="+url.QueryEscape("branch:"+branch) {
		t.Fatalf("active origin links = %#v", links)
	}
	head, comparisonBase := strings.Repeat("a", 40), strings.Repeat("b", 40)
	archived := provider.Links(origin, branch, base, comparisonBase, head, true)
	if archived.CompareURL != origin.URL+"/compare/"+comparisonBase+"..."+head ||
		archived.BranchURL != origin.URL+"/tree/"+head ||
		provider.CommitURL(origin, head) != origin.URL+"/commit/"+head {
		t.Fatalf("archived origin links = %#v", archived)
	}
}

func TestGitHubProviderUsesExactHeadAndRejectsForeignWorkflowLinks(t *testing.T) {
	head := strings.Repeat("a", 40)
	stale := strings.Repeat("b", 40)
	var commands [][]string
	provider := githubOriginProvider{executable: "gh-test", command: func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, append([]string{name}, args...))
		if name != "gh-test" {
			return nil, errors.New("wrong transport")
		}
		if len(args) > 1 && args[0] == "api" && args[1] == "graphql" {
			return []byte(`{"data":{"repository":{"defaultBranchRef":{"name":"main"},"ref":{"target":{"oid":"` + head + `"}}}}}`), nil
		}
		if len(args) > 1 && args[0] == "api" {
			return []byte(`{"status":"ahead"}`), nil
		}
		return json.Marshal([]Run{
			{WorkflowName: "valid", HeadSHA: head, URL: "https://github.com/example/project/actions/runs/123"},
			{WorkflowName: "foreign", HeadSHA: head, URL: "https://other.test/example/project/actions/runs/124"},
			{WorkflowName: "stale", HeadSHA: stale, URL: "https://github.com/example/project/actions/runs/125"},
		})
	}}
	origin, _ := provider.Resolve("example/project")
	repository, err := provider.Repository(context.Background(), origin, "feature")
	if err != nil || repository.DefaultBranch != "main" || repository.HeadSHA != head {
		t.Fatalf("remote repository = %#v, %v", repository, err)
	}
	comparison, err := provider.CompareHeads(context.Background(), origin, head, stale)
	if err != nil || comparison != PushStatusRemoteAhead {
		t.Fatalf("remote comparison = %q, %v", comparison, err)
	}
	runs, err := provider.Workflows(context.Background(), origin, "feature", head)
	if err != nil || len(runs) != 2 || runs[0].URL != origin.URL+"/actions/runs/123" || runs[1].URL != "" {
		t.Fatalf("exact-head workflows = %#v, %v", runs, err)
	}
	if !reflect.DeepEqual(commands[2], []string{"gh-test", "run", "list", "-R", "example/project", "--branch", "feature", "--limit", "100",
		"--json", "workflowName,status,conclusion,headSha,url", "--commit", head}) {
		t.Fatalf("workflow command = %#v", commands[2])
	}
}

func TestOriginFailureKeepsLocalReviewAndLegacyJSON(t *testing.T) {
	fixture := newRepositoryFixture(t)
	head := commitInWorktree(t, fixture.worktree, "local")
	registration := activeRepository()
	provider := githubOriginProvider{executable: "gh-test", command: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("GitHub unavailable"), errors.New("offline")
	}}
	status := (Runner{Workspace: fixture.workspace, Provider: provider}).Inspect(
		context.Background(), testSlug, []session.Repository{registration}, false)[0]
	if status.LocalHeadSHA != head || status.Origin == nil || status.OriginError != "GitHub unavailable" ||
		status.GitHubError != status.OriginError || status.PushStatus != PushStatusUnknown ||
		status.CompareURL != status.OriginLinks.CompareURL || status.BranchURL != status.OriginLinks.BranchURL {
		t.Fatalf("origin failure lost local status or aliases: %#v", status)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"github", "githubError", "origin", "originLinks", "originError", "branchUrl", "compareUrl", "actionsUrl"} {
		if len(fields[key]) == 0 {
			t.Fatalf("missing compatible status field %q: %s", key, encoded)
		}
	}
	reader := ReviewReader{Workspace: fixture.workspace}
	for _, github := range []string{"", "../project", "example/project"} {
		registration.GitHub = github
		repo, err := reader.Resolve(context.Background(), testSlug, registration, false)
		if err != nil {
			t.Fatalf("local review with origin %q: %v", github, err)
		}
		if github != "example/project" && repo.Origin != nil {
			t.Fatalf("invalid or absent origin acquired links: %#v", repo.Origin)
		}
		history, err := reader.History(context.Background(), repo, ReviewPair{Base: fixture.baseHead, Head: head}, 0)
		if err != nil || len(history.Commits) != 1 {
			t.Fatalf("local history with origin %q: %#v, %v", github, history, err)
		}
		if (history.Commits[0].URL != "") != (repo.Origin != nil) {
			t.Fatalf("unexpected commit link with origin %q: %#v", github, history.Commits[0])
		}
	}
}
