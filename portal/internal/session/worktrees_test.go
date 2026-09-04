package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestActiveRepositoriesDiscoversCanonicalUnregisteredWorktree(t *testing.T) {
	workspace := t.TempDir()
	repository := filepath.Join(workspace, "repos", "vpsadminos.git")
	worktree := filepath.Join(workspace, "worktrees", "2026-09-04-test", "vpsadminos")
	if err := os.MkdirAll(filepath.Dir(repository), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, "init", "--bare", "--initial-branch=master", repository)
	runGit(t, "--git-dir="+repository, "config", "remote.origin.url", "git@github.com:vpsfreecz/vpsadminos.git")
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
	if len(repositories) != 1 || repositories[0].Name != "vpsadminos" ||
		repositories[0].Project != "vpsadminos" || repositories[0].GitHub != "vpsfreecz/vpsadminos" {
		t.Fatalf("repositories = %#v", repositories)
	}
}

func TestMergeActiveRepositoriesRejectsConflictingRegistration(t *testing.T) {
	registered := []Repository{{
		Name: "vpsadminos", Project: "vpsadminos", Branch: "feature", GitHub: "vpsfreecz/vpsadminos",
	}}
	discovered := []Repository{{
		Name: "vpsadminos", Project: "vpsadminos", Branch: "different", GitHub: "vpsfreecz/vpsadminos",
	}}

	result, err := MergeActiveRepositories(registered, discovered)
	if err == nil {
		t.Fatal("expected conflicting registration to be rejected")
	}
	if len(result) != 1 || result[0].Branch != "feature" {
		t.Fatalf("registered repository was replaced: %#v", result)
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
