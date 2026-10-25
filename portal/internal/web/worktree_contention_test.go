package web

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

// Use the real Runner and Git. Only the absent external terminal is injected;
// no installed workspace, provider configuration or native process is selected.
const worktreeContentionRuby = `
require 'json'
load ARGV.shift
workspace, authority, slug, waiting, shell = ARGV
class NoWorktreeTerminal
  def session(_slug); nil; end
end
class WorktreeContentionRunner < DevSession::Runner
  attr_accessor :waiting_file
  def with_host_lock(path, description, **options, &block)
    options[:on_wait] = lambda do
      stat = File.stat(path)
      File.write(waiting_file + '.tmp', JSON.generate(pid: Process.pid, device: stat.dev, inode: stat.ino))
      File.rename(waiting_file + '.tmp', waiting_file)
    end
    super(path, description, **options, &block)
  end
end
runner = WorktreeContentionRunner.new(workspace:, authority_dir: authority,
  tmux: NoWorktreeTerminal.new, portal_command: [], require_runtime: false,
  env: { 'PATH' => '', 'SHELL' => shell, 'XDG_STATE_HOME' => File.join(workspace, '.state'),
         'DEV_WORKSPACES_CODEX_PROVIDERS' => File.join(workspace, 'absent-providers.json') })
runner.waiting_file = waiting
runner.worktree_add(slug, 'sample', as_is: true, name: nil, branch: nil, base: 'master', fetch: false)
`

func TestWorktreeRegistrationDistinguishesPortalReadersFromWriters(t *testing.T) {
	for _, holder := range []string{"portal-reader", "writer"} {
		t.Run(holder, func(t *testing.T) {
			ruby, err := exec.LookPath("ruby")
			if err != nil {
				t.Skip("Ruby is unavailable")
			}
			if _, err := exec.LookPath("git"); err != nil {
				t.Skip("Git is unavailable")
			}
			shell, err := exec.LookPath("sh")
			if err != nil {
				t.Fatal(err)
			}
			source, err := filepath.Abs("../../../libexec/dev-session")
			if err != nil {
				t.Fatal(err)
			}
			s := newTestServer(t)
			prepareInteractiveConversation(t, s, "source")
			s.config.Codex = &browserContractCodex{transcript: codex.Transcript{ThreadID: "thread-1", Status: "idle"}}
			seed := filepath.Join(t.TempDir(), "seed")
			runWebGit(t, "init", "-b", "master", seed)
			runWebGit(t, "-C", seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "fixture")
			runWebGit(t, "clone", "--bare", seed, filepath.Join(s.config.Workspace, "repos", "sample.git"))
			var release func()
			if holder == "portal-reader" {
				release = holdCreationConversation(t, s, "source")
			} else {
				lock, err := session.LockRuntimeExclusive(s.config.AuthorityDir, "source")
				if err != nil {
					t.Fatal(err)
				}
				release = func() { _ = lock.Close() }
			}
			defer release()
			waiting := filepath.Join(t.TempDir(), "waiting.json")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, ruby, "-e", worktreeContentionRuby, source, s.config.Workspace, s.config.AuthorityDir, "source", waiting, shell)
			command.Env = []string{"PATH=" + os.Getenv("PATH")}
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			checkout := filepath.Join(s.config.Workspace, "worktrees", "source", "sample")
			if holder == "writer" {
				if err := <-done; err == nil || !strings.Contains(output.String(), "another dev-session command") {
					t.Fatalf("competing writer was not refused: %v %s", err, &output)
				}
			} else {
				for {
					if _, err := os.Stat(waiting); err == nil {
						break
					}
					select {
					case err := <-done:
						t.Fatalf("portal reader prevented registration: %v %s", err, &output)
					case <-time.After(10 * time.Millisecond):
					}
				}
				var event struct{ Device, Inode uint64 }
				if err := json.Unmarshal(acceptedForkRead(t, waiting), &event); err != nil {
					t.Fatal(err)
				}
				var stat unix.Stat_t
				if err := unix.Stat(filepath.Join(s.config.AuthorityDir, "source.lock"), &stat); err != nil || stat.Dev != event.Device || stat.Ino != event.Inode {
					t.Fatalf("Ruby waited on another inode: %+v %+v %v", event, stat, err)
				}
				if _, err := os.Stat(checkout); !os.IsNotExist(err) {
					t.Fatal("registration mutated the checkout before acquiring EX")
				}
				other := exec.CommandContext(ctx, ruby, "-e", worktreeContentionRuby, source, s.config.Workspace, s.config.AuthorityDir, "unrelated", waiting+"-other", shell)
				other.Env = command.Env
				if data, err := other.CombinedOutput(); err != nil {
					t.Fatalf("reader wait blocked unrelated registration: %v %s", err, data)
				}
				release()
				if err := <-done; err != nil {
					t.Fatalf("registration did not finish after reader release: %v %s", err, &output)
				}
				if got := strings.TrimSpace(webGitOutput(t, "-C", checkout, "branch", "--show-current")); got != "source" {
					t.Fatal(got)
				}
				return
			}
			if _, err := os.Stat(checkout); !os.IsNotExist(err) {
				t.Fatal("refused registration mutated the checkout")
			}
		})
	}
}
