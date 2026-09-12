package session

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	paths, err := filepath.Glob(filepath.Join(stateRoot, "removed", "*", "*", "recovery.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("recovery marker paths: %v, %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var marker map[string]any
	if err := json.Unmarshal(data, &marker); err != nil {
		t.Fatal(err)
	}
	expected := fmt.Sprintf("%x", sha256.Sum256([]byte(operationID)))
	for _, clock := range []string{"2000-01-01T00:00:00Z", "2099-01-01T00:00:00Z"} {
		marker["removed_at"] = clock
		encoded, _ := json.Marshal(marker)
		if err := os.WriteFile(paths[0], encoded, 0600); err != nil {
			t.Fatal(err)
		}
		history, err := CompletedRemovalHistory(workspace, "example", stateRoot)
		if err != nil || history != expected {
			t.Fatalf("history depends on clock: %q, %v", history, err)
		}
	}
	// Duplicate markers for one operation do not grow the identity set.
	for _, id := range []string{operationID, strings.Repeat("b", 64)} {
		directory := filepath.Join(filepath.Dir(filepath.Dir(paths[0])), id+"-example-copy")
		if err := os.MkdirAll(filepath.Join(directory, "work"), 0700); err != nil {
			t.Fatal(err)
		}
		marker["recovery"], marker["operation_id"] = directory, id
		encoded, _ := json.Marshal(marker)
		if err := os.WriteFile(filepath.Join(directory, "recovery.json"), encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	expected = fmt.Sprintf("%x", sha256.Sum256([]byte(operationID+"\n"+strings.Repeat("b", 64))))
	history, err := CompletedRemovalHistory(workspace, "example", stateRoot)
	if err != nil || history != expected {
		t.Fatalf("sorted operation history = %q, %v", history, err)
	}
	rubyHistory := `load ARGV.fetch(0); runner = DevSession::Runner.new(workspace: ARGV.fetch(1), env: ENV.to_h.merge('DEV_WORKSPACES_STATE' => ARGV.fetch(2))); print runner.send(:completed_removal_history, 'example')`
	if output, err := exec.Command("ruby", "-e", rubyHistory, devSession, workspace, stateRoot).CombinedOutput(); err != nil || string(output) != expected {
		t.Fatalf("Ruby and Go history differ: %q, %v", output, err)
	}
	mismatched, err := CompletedRemoval(
		workspace, "example", stateRoot,
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		startedAt,
	)
	if err != nil {
		t.Fatalf("check a mismatched removal identity: %v", err)
	}
	if mismatched {
		t.Fatal("terminal removal marker matched a different operation identity")
	}
	// A different old operation must not broaden the exact confirmation API's
	// tracking requirements, while the full history still fails closed.
	oldDirectory := filepath.Join(filepath.Dir(filepath.Dir(paths[0])), "000-example-old")
	if err := os.Mkdir(oldDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	marker["recovery"], marker["operation_id"] = oldDirectory, strings.Repeat("d", 64)
	encoded, _ := json.Marshal(marker)
	if err := os.WriteFile(filepath.Join(oldDirectory, "recovery.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	completed, err = CompletedRemoval(workspace, "example", stateRoot, operationID, startedAt)
	if err != nil || !completed {
		t.Fatalf("unrelated missing tracking prevented exact confirmation: %v, %v", completed, err)
	}
	if _, err := CompletedRemovalHistory(workspace, "example", stateRoot); err == nil {
		t.Fatal("history accepted completed removal without preserved tracking")
	}
}
