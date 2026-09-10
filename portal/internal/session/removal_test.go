package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCompletedRemovalConsumesDevSessionMarker(t *testing.T) {
	workspaceRoot := filepath.Clean(filepath.Join("..", "..", ".."))
	devSession := filepath.Join(workspaceRoot, "libexec", "dev-session")
	if _, err := os.Stat(devSession); err != nil {
		t.Fatalf("locate dev-session producer: %v", err)
	}

	workspace := filepath.Join(t.TempDir(), "workspace")
	stateHome := filepath.Join(t.TempDir(), "state")
	stateRoot := filepath.Join(stateHome, "previous-workspaces")
	if err := os.MkdirAll(filepath.Join(workspace, "work", "example"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "worktrees", ".locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	operationID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	script := `
load ARGV.fetch(0)
workspace = ARGV.fetch(1)
state_home = ARGV.fetch(2)
slug = ARGV.fetch(3)
env = ENV.to_h.merge(
  'XDG_STATE_HOME' => state_home,
  'DEV_WORKSPACES_NAMESPACE' => 'previous-workspaces'
)
env['PATH'] = File.dirname(env.fetch('SHELL'))
runner = DevSession::Runner.new(workspace: workspace, env: env)
operation_id = ARGV.fetch(4)
removal = runner.send(:prepare_removal!, slug, force: false, operation_id: operation_id)
%w[
  validated thread_retiring thread_retired clusters_released
  worktrees_removed runtime_retired
].each { |phase| runner.send(:advance_removal!, slug, removal, phase) }
runner.send(:preserve_removed_state!, slug, removal, nil)
runner.send(:advance_removal!, slug, removal, 'tracking_preserved')
runner.send(:advance_removal!, slug, removal, 'tracking_committed')
runner.send(:finalize_removal!, slug, removal)
`
	command := exec.Command(
		"ruby", "-e", script, devSession, workspace, stateHome, "example", operationID,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("produce terminal removal marker: %v\n%s", err, output)
	}

	completed, err := CompletedRemoval(workspace, "example", stateRoot, operationID, startedAt)
	if err != nil {
		t.Fatalf("consume terminal removal marker: %v", err)
	}
	if !completed {
		t.Fatal("Ruby-produced terminal removal marker was not recognized")
	}
	mismatched, err := CompletedRemoval(
		workspace, "example", stateRoot,
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		startedAt,
	)
	if err != nil {
		t.Fatalf("check a mismatched removal identity: %v", err)
	}
	if mismatched {
		t.Fatal("terminal removal marker matched a different operation identity")
	}
}
