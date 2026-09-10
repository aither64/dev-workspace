package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestActiveRepositoriesDiscoversCanonicalUnregisteredWorktree(t *testing.T) {
	workspace := t.TempDir()
	repository := filepath.Join(workspace, "repos", "beta.git")
	worktree := filepath.Join(workspace, "worktrees", "2026-09-04-test", "beta")
	if err := os.MkdirAll(filepath.Dir(repository), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, "init", "--bare", "--initial-branch=master", repository)
	runGit(t, "--git-dir="+repository, "config", "remote.origin.url", "git@github.com:example-org/beta.git")
	runGit(t, "--git-dir="+repository, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")
	seed := t.TempDir()
	runGit(t, "init", "--initial-branch=master", seed)
	runGit(t, "-C", seed, "config", "user.email", "test@example.invalid")
	runGit(t, "-C", seed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", seed, "add", "README")
	runGit(t, "-C", seed, "commit", "-m", "seed")
	head := gitOutput(t, "-C", seed, "rev-parse", "HEAD")
	runGit(t, "--git-dir="+repository, "fetch", seed, head+":refs/heads/2026-09-04-test")
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, "--git-dir="+repository, "worktree", "add", worktree, "2026-09-04-test")

	repositories, err := ActiveRepositories(workspace, "2026-09-04-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || repositories[0].Name != "beta" ||
		repositories[0].Project != "beta" || repositories[0].GitHub != "example-org/beta" {
		t.Fatalf("repositories = %#v", repositories)
	}
}

func TestMergeActiveRepositoriesRejectsConflictingRegistration(t *testing.T) {
	registered := []Repository{{
		Name: "beta", Project: "beta", Branch: "feature", GitHub: "example-org/beta",
	}}
	discovered := []Repository{{
		Name: "beta", Project: "beta", Branch: "different", GitHub: "example-org/beta",
	}}

	result, err := MergeActiveRepositories(registered, discovered)
	if err == nil {
		t.Fatal("expected conflicting registration to be rejected")
	}
	if len(result) != 1 || result[0].Branch != "feature" {
		t.Fatalf("registered repository was replaced: %#v", result)
	}
}

func TestMergeActiveRepositoriesAcceptsVerifiedProjectAlias(t *testing.T) {
	registered := []Repository{{
		Name: "example-kb-contracts", Project: "example-kb-contracts",
		Branch: "2026-08-18-alpha-password-reset", GitHub: "example-org/example-kb-contracts",
	}}
	discovered := []Repository{{
		Name: "example-kb-contracts", Project: "alpha-kb-captures",
		Branch: "2026-08-18-alpha-password-reset", GitHub: "example-org/example-kb-contracts",
	}}

	result, err := MergeActiveRepositories(registered, discovered)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].Project != "example-kb-contracts" {
		t.Fatalf("registered repository was not preserved: %#v", result)
	}
}

func TestMergeActiveRepositoriesRejectsUnverifiedProjectAlias(t *testing.T) {
	testCases := []struct {
		name       string
		branch     string
		registered string
		discovered string
	}{
		{name: "missing branch", registered: "example-org/example-kb-contracts", discovered: "example-org/example-kb-contracts"},
		{name: "different branch", branch: "different", registered: "example-org/example-kb-contracts", discovered: "example-org/example-kb-contracts"},
		{name: "missing registered GitHub", branch: "feature", discovered: "example-org/example-kb-contracts"},
		{name: "missing discovered GitHub", branch: "feature", registered: "example-org/example-kb-contracts"},
		{name: "different GitHub", branch: "feature", registered: "example-org/example-kb-contracts", discovered: "example-org/other"},
		{name: "invalid GitHub", branch: "feature", registered: "not a repository", discovered: "not a repository"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			registered := []Repository{{
				Name: "example-kb-contracts", Project: "example-kb-contracts",
				Branch: "feature", GitHub: testCase.registered,
			}}
			discovered := []Repository{{
				Name: "example-kb-contracts", Project: "alpha-kb-captures",
				Branch: testCase.branch, GitHub: testCase.discovered,
			}}

			if _, err := MergeActiveRepositories(registered, discovered); err == nil {
				t.Fatal("expected unverified project alias to be rejected")
			}
		})
	}
}

func TestDiscoverActiveRepositoriesHonorsCallerDeadline(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := DiscoverActiveRepositoriesContext(ctx, workspace)
	if err == nil {
		t.Fatal("blocked Git discovery ignored the caller deadline")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Git discovery exceeded its caller deadline: %s", elapsed)
	}
}

func runGit(t *testing.T, args ...string) {
	t.Helper()
	if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
}

func gitOutput(t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(output[:len(output)-1])
}
