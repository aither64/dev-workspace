package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/vpsfree-cz-workspace/portal/internal/session"
)

const (
	testSlug    = "2026-09-09-status"
	testProject = "project"
	testBranch  = "feature"
)

type repositoryFixture struct {
	workspace string
	commonDir string
	worktree  string
	baseHead  string
}

func TestActiveRepositoryExactlyPushedUsesCanonicalWorktreeAndExactRuns(t *testing.T) {
	fixture := newRepositoryFixture(t)
	localHead := commitInWorktree(t, fixture.worktree, "local")
	runs, err := json.Marshal([]Run{
		{WorkflowName: "current", Status: "completed", Conclusion: "success", HeadSHA: localHead, URL: "https://example.test/current"},
		{WorkflowName: "queued", Status: "queued", HeadSHA: localHead, URL: "https://example.test/queued"},
		{WorkflowName: "running", Status: "in_progress", HeadSHA: localHead, URL: "https://example.test/running"},
		{WorkflowName: "stale", Status: "completed", Conclusion: "failure", HeadSHA: strings.Repeat("f", 40), URL: "https://example.test/stale"},
	})
	if err != nil {
		t.Fatal(err)
	}
	gh, logPath := fakeGH(t, localHead, "", string(runs), false)

	statuses := (Runner{Workspace: fixture.workspace, GH: gh}).Inspect(
		context.Background(), testSlug, []session.Repository{activeRepository()}, false,
	)
	if len(statuses) != 1 {
		t.Fatalf("statuses = %#v", statuses)
	}
	status := statuses[0]
	if status.Project != testProject || status.DefaultBranch != "main" ||
		status.LocalHeadSHA != localHead || status.RemoteHeadSHA != localHead ||
		status.PushStatus != PushStatusExactlyPushed || status.StatusError != "" ||
		status.GitHubError != "" {
		t.Fatalf("unexpected status: %#v", status)
	}
	if len(status.Runs) != 3 || status.Runs[0].WorkflowName != "current" ||
		status.Runs[1].Status != "queued" || status.Runs[2].Status != "in_progress" ||
		status.Runs[0].HeadSHA != localHead {
		t.Fatalf("workflow runs were not filtered to the exact head: %#v", status.Runs)
	}
	commands := readFile(t, logPath)
	if !strings.Contains(commands, "run list -R example/project --branch feature") ||
		!strings.Contains(commands, "--limit 100") || !strings.Contains(commands, "--commit "+localHead) {
		t.Fatalf("exact-head run filter was not passed to gh:\n%s", commands)
	}
}

func TestActiveRepositoryClassifiesHeadRelationships(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*testing.T, repositoryFixture) string
		wantStatus string
	}{
		{
			name: "missing remote branch",
			prepare: func(t *testing.T, fixture repositoryFixture) string {
				commitInWorktree(t, fixture.worktree, "local")
				return ""
			},
			wantStatus: PushStatusNotPushed,
		},
		{
			name: "local ahead",
			prepare: func(t *testing.T, fixture repositoryFixture) string {
				commitInWorktree(t, fixture.worktree, "local")
				return fixture.baseHead
			},
			wantStatus: PushStatusNotPushed,
		},
		{
			name: "remote ahead",
			prepare: func(t *testing.T, fixture repositoryFixture) string {
				return commitFrom(t, fixture, fixture.baseHead, "remote")
			},
			wantStatus: PushStatusRemoteAhead,
		},
		{
			name: "divergent",
			prepare: func(t *testing.T, fixture repositoryFixture) string {
				commitInWorktree(t, fixture.worktree, "local")
				return commitFrom(t, fixture, fixture.baseHead, "remote")
			},
			wantStatus: PushStatusDivergent,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRepositoryFixture(t)
			remoteHead := test.prepare(t, fixture)
			gh, logPath := fakeGH(t, remoteHead, "", "[]", false)
			status := (Runner{Workspace: fixture.workspace, GH: gh}).Inspect(
				context.Background(), testSlug, []session.Repository{activeRepository()}, false,
			)[0]
			if status.PushStatus != test.wantStatus || status.StatusError != "" || status.GitHubError != "" {
				t.Fatalf("unexpected status: %#v", status)
			}
			if status.LocalHeadSHA == "" || status.RemoteHeadSHA != remoteHead {
				t.Fatalf("unexpected heads: %#v", status)
			}
			if strings.Contains(readFile(t, logPath), "run list") || len(status.Runs) != 0 {
				t.Fatalf("workflow runs were queried for mismatched heads: %#v", status)
			}
		})
	}
}

func TestActiveRepositoryFallsBackToAuthoritativeGitHubComparison(t *testing.T) {
	fixture := newRepositoryFixture(t)
	remoteHead := strings.Repeat("f", 40)
	gh, logPath := fakeGH(t, remoteHead, "ahead", "[]", false)
	status := (Runner{Workspace: fixture.workspace, GH: gh}).Inspect(
		context.Background(), testSlug, []session.Repository{activeRepository()}, false,
	)[0]
	if status.PushStatus != PushStatusRemoteAhead || status.StatusError != "" || status.GitHubError != "" {
		t.Fatalf("unexpected comparison fallback: %#v", status)
	}
	commands := readFile(t, logPath)
	if !strings.Contains(commands, "api repos/example/project/compare/"+fixture.baseHead+"..."+remoteHead) {
		t.Fatalf("GitHub comparison was not requested:\n%s", commands)
	}
	if strings.Contains(commands, "run list") {
		t.Fatalf("workflow runs were queried for mismatched heads:\n%s", commands)
	}
}

func TestActiveRepositoryReportsUnknownForUnprovableHeads(t *testing.T) {
	fixture := newRepositoryFixture(t)
	remoteHead := strings.Repeat("f", 40)
	gh, _ := fakeGH(t, remoteHead, "", "[]", false)
	status := (Runner{Workspace: fixture.workspace, GH: gh}).Inspect(
		context.Background(), testSlug, []session.Repository{activeRepository()}, false,
	)[0]
	if status.PushStatus != PushStatusUnknown || status.StatusError == "" || status.GitHubError != "" {
		t.Fatalf("unexpected unknown status: %#v", status)
	}
}

func TestActiveRepositoryRejectsUnexpectedCheckedOutBranch(t *testing.T) {
	fixture := newRepositoryFixture(t)
	runGit(t, "-C", fixture.worktree, "switch", "-c", "other")
	localHead := gitOutput(t, "-C", fixture.worktree, "rev-parse", "HEAD")
	gh, logPath := fakeGH(t, localHead, "", "[]", false)
	status := (Runner{Workspace: fixture.workspace, GH: gh}).Inspect(
		context.Background(), testSlug, []session.Repository{activeRepository()}, false,
	)[0]
	if status.PushStatus != PushStatusUnknown || status.LocalHeadSHA != "" ||
		status.RemoteHeadSHA != localHead || !strings.Contains(status.StatusError, "recorded feature branch") {
		t.Fatalf("unexpected mismatched-worktree status: %#v", status)
	}
	if strings.Contains(readFile(t, logPath), "run list") {
		t.Fatal("workflow runs were queried without a verified local head")
	}
}

func TestActiveRepositoryRejectsWorktreeFromAnotherRepository(t *testing.T) {
	workspace := t.TempDir()
	commonDir := filepath.Join(workspace, "repos", testProject+".git")
	if err := os.MkdirAll(filepath.Dir(commonDir), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, "init", "--bare", "--initial-branch=master", commonDir)
	worktree := filepath.Join(workspace, "worktrees", testSlug, testProject)
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, "init", "--initial-branch="+testBranch, worktree)
	configureGitUser(t, worktree)
	localHead := commitInWorktree(t, worktree, "local")
	gh, logPath := fakeGH(t, localHead, "", "[]", false)

	status := (Runner{Workspace: workspace, GH: gh}).Inspect(
		context.Background(), testSlug, []session.Repository{activeRepository()}, false,
	)[0]
	if status.PushStatus != PushStatusUnknown || status.LocalHeadSHA != "" ||
		!strings.Contains(status.StatusError, "unexpected repository") {
		t.Fatalf("unexpected foreign-worktree status: %#v", status)
	}
	if strings.Contains(readFile(t, logPath), "run list") {
		t.Fatal("workflow runs were queried for a noncanonical worktree")
	}
}

func TestActiveRepositoryKeepsRecordedDefaultBranchWhenGitHubLookupFails(t *testing.T) {
	fixture := newRepositoryFixture(t)
	gh, _ := fakeGH(t, "", "", "[]", true)
	status := (Runner{Workspace: fixture.workspace, GH: gh}).Inspect(
		context.Background(), testSlug, []session.Repository{activeRepository()}, false,
	)[0]
	if status.DefaultBranch != "master" ||
		status.CompareURL != "https://github.com/example/project/compare/master...feature" ||
		status.LocalHeadSHA != fixture.baseHead || status.PushStatus != PushStatusUnknown ||
		status.GitHubError == "" {
		t.Fatalf("unexpected fallback status: %#v", status)
	}
}

func TestArchivedComparisonUsesImmutableManifestCommits(t *testing.T) {
	base := strings.Repeat("1", 40)
	head := strings.Repeat("2", 40)
	archivedRuns := `[{
		"workflowName":"archived","status":"completed","conclusion":"success",
		"headSha":"` + head + `","url":"https://example.test/archived"
	},{
		"workflowName":"newer","status":"completed","conclusion":"failure",
		"headSha":"` + strings.Repeat("3", 40) + `","url":"https://example.test/newer"
	}]`
	gh, logPath := fakeGH(t, "", "", archivedRuns, false)
	statuses := (Runner{GH: gh}).Inspect(context.Background(), "", []session.Repository{{
		Name: "project", Project: "project", GitHub: "example/project", Branch: "feature",
		DefaultBranch: "master", InitialBaseSHA: base, FinalHeadSHA: head,
	}}, true)
	if len(statuses) != 1 || statuses[0].CompareURL != "https://github.com/example/project/compare/"+base+"..."+head ||
		statuses[0].BranchURL != "https://github.com/example/project/tree/"+head ||
		statuses[0].HeadSHA != head || statuses[0].LocalHeadSHA != "" || statuses[0].RemoteHeadSHA != "" ||
		statuses[0].PushStatus != "" || len(statuses[0].Runs) != 1 {
		t.Fatalf("unexpected archived status: %#v", statuses)
	}
	commands := readFile(t, logPath)
	if strings.Contains(commands, " api ") || !strings.Contains(commands, "--commit "+head) {
		t.Fatalf("archived enrichment changed its immutable behavior:\n%s", commands)
	}
}

func TestArchivedRepositoryWithoutFinalHeadOmitsRuns(t *testing.T) {
	gh, logPath := fakeGH(t, "", "", `[{"headSha":"`+strings.Repeat("3", 40)+`"}]`, false)
	status := (Runner{GH: gh}).Inspect(context.Background(), "", []session.Repository{{
		Name: "project", Project: "project", GitHub: "example/project", Branch: "feature",
		DefaultBranch: "master",
	}}, true)[0]
	if len(status.Runs) != 0 || status.GitHubError != "" {
		t.Fatalf("unexpected archived status without a final head: %#v", status)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("gh was invoked without an immutable final head: %v", err)
	}
}

func TestRepositoryCommandsHaveAnInternalDeadline(t *testing.T) {
	fixture := newRepositoryFixture(t)
	directory := t.TempDir()
	gh := filepath.Join(directory, "gh")
	writeExecutable(t, gh, `#!/bin/sh
exec sleep 5
`)
	started := time.Now()
	status := (Runner{
		Workspace: fixture.workspace, GH: gh, CommandTimeout: 50 * time.Millisecond,
	}).Inspect(context.Background(), testSlug, []session.Repository{activeRepository()}, false)[0]
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("repository inspection ignored its deadline: %s", elapsed)
	}
	if status.PushStatus != PushStatusUnknown || !strings.Contains(status.GitHubError, "deadline exceeded") {
		t.Fatalf("unexpected timeout status: %#v", status)
	}
}

func TestGitHubEnrichmentRunsConcurrently(t *testing.T) {
	directory := t.TempDir()
	gh := filepath.Join(directory, "gh")
	writeExecutable(t, gh, `#!/bin/sh
sleep 1
printf '[]\n'
`)
	statuses := []Status{
		{Name: "one", GitHub: "example/one", Branch: "feature", HeadSHA: strings.Repeat("1", 40), immutable: true},
		{Name: "two", GitHub: "example/two", Branch: "feature", HeadSHA: strings.Repeat("2", 40), immutable: true},
	}
	started := time.Now()
	(Runner{GH: gh}).EnrichAll(context.Background(), statuses)
	if elapsed := time.Since(started); elapsed > 1800*time.Millisecond {
		t.Fatalf("enrichment was serialized: %s", elapsed)
	}
	for _, status := range statuses {
		if status.GitHubError != "" || len(status.Runs) != 0 {
			t.Fatalf("unexpected status: %#v", status)
		}
	}
}

func activeRepository() session.Repository {
	return session.Repository{
		Name: testProject, Project: testProject, GitHub: "example/project",
		Branch: testBranch, DefaultBranch: "master",
	}
}

func newRepositoryFixture(t *testing.T) repositoryFixture {
	t.Helper()
	workspace := t.TempDir()
	seed := filepath.Join(t.TempDir(), "seed")
	runGit(t, "init", "--initial-branch=master", seed)
	configureGitUser(t, seed)
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", seed, "add", "README")
	runGit(t, "-C", seed, "commit", "-m", "base")
	baseHead := gitOutput(t, "-C", seed, "rev-parse", "HEAD")

	commonDir := filepath.Join(workspace, "repos", testProject+".git")
	if err := os.MkdirAll(filepath.Dir(commonDir), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, "clone", "--bare", seed, commonDir)
	runGit(t, "--git-dir="+commonDir, "branch", testBranch, baseHead)
	worktree := filepath.Join(workspace, "worktrees", testSlug, testProject)
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, "--git-dir="+commonDir, "worktree", "add", worktree, testBranch)
	configureGitUser(t, worktree)

	return repositoryFixture{
		workspace: workspace, commonDir: commonDir, worktree: worktree, baseHead: baseHead,
	}
}

func commitInWorktree(t *testing.T, worktree, label string) string {
	t.Helper()
	path := filepath.Join(worktree, label)
	if err := os.WriteFile(path, []byte(label+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", worktree, "add", label)
	runGit(t, "-C", worktree, "commit", "-m", label)
	return gitOutput(t, "-C", worktree, "rev-parse", "HEAD")
}

func commitFrom(t *testing.T, fixture repositoryFixture, parent, label string) string {
	t.Helper()
	branch := "test-" + label
	path := filepath.Join(t.TempDir(), "worktree")
	runGit(t, "--git-dir="+fixture.commonDir, "worktree", "add", "-b", branch, path, parent)
	configureGitUser(t, path)
	head := commitInWorktree(t, path, label)
	runGit(t, "--git-dir="+fixture.commonDir, "worktree", "remove", path)
	return head
}

func fakeGH(t *testing.T, remoteHead, compareStatus, runs string, failRepository bool) (string, string) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "gh")
	logPath := filepath.Join(directory, "commands.log")
	ref := "null"
	if remoteHead != "" {
		ref = fmt.Sprintf(`{"target":{"oid":%q}}`, remoteHead)
	}
	repository := fmt.Sprintf(`{"defaultBranchRef":{"name":"main"},"ref":%s}`, ref)
	apiFailure := ""
	if failRepository {
		apiFailure = "printf 'GitHub unavailable\\n' >&2\n        exit 1"
	}
	compare := "printf 'comparison unavailable\\n' >&2\n        exit 1"
	if compareStatus != "" {
		compare = fmt.Sprintf("printf '%%s\\n' '%s'", fmt.Sprintf(`{"status":%q}`, compareStatus))
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
case "$1" in
  api)
    case "$2" in
      graphql)
        %s
        printf '%%s\n' '%s'
        ;;
      repos/*/compare/*)
        %s
        ;;
      *) exit 2 ;;
    esac
    ;;
  run)
    printf '%%s\n' '%s'
    ;;
  *) exit 2 ;;
esac
`, logPath, apiFailure, fmt.Sprintf(`{"data":{"repository":%s}}`, repository), compare, runs)
	writeExecutable(t, path, script)
	return path, logPath
}

func configureGitUser(t *testing.T, path string) {
	t.Helper()
	runGit(t, "-C", path, "config", "user.email", "test@example.invalid")
	runGit(t, "-C", path, "config", "user.name", "Test")
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func runGit(t *testing.T, args ...string) {
	t.Helper()
	if output, err := runCommand("git", args...); err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
}

func gitOutput(t *testing.T, args ...string) string {
	t.Helper()
	output, err := runCommand("git", args...)
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
	return strings.TrimSpace(string(output))
}

func runCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}
