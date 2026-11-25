package session

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompletedRemovalForTargetUsesOneExactRetainedCandidate(t *testing.T) {
	for _, fault := range []string{"none", "unstarted-null", "legacy-absent", "wrong-root", "wrong-capture", "expected-absent", "capture-absent", "capture-null", "capture-malformed", "mixed-pi", "phase", "time", "operation", "manifest-missing", "manifest-root", "duplicate", "split-candidates", "unrelated-incomplete"} {
		t.Run(fault, func(t *testing.T) {
			workspace, stateRoot, slug := t.TempDir(), t.TempDir(), "example"
			operation := strings.Repeat("a", 64)
			notBefore := time.Now().UTC()
			root := userstate.WorkspaceDirectory(stateRoot, "removed", workspace)
			directory := filepath.Join(root, "one-example-candidate")
			if err := os.MkdirAll(filepath.Join(directory, "work"), 0700); err != nil {
				t.Fatal(err)
			}
			original, native := "origin-member", "current-member"
			targets := &CodexTargets{Incarnation: strings.Repeat("b", 64), Revision: ^uint64(0), ThreadID: "current-root",
				Members: []CodexTargetMember{{Member: "implementer0", OriginalThreadID: &original, ThreadID: &native}}}
			expected, originalRoot := targets.Clone(), "origin-root"
			marker := map[string]any{"schema": 1, "slug": slug, "workspace": workspace, "operation_id": operation, "state": "removed", "phase": "removed",
				"tracking": "work", "recovery": directory, "removed_at": notBefore.Add(time.Second).Format(time.RFC3339Nano), "codex_targets": targets}
			manifest := "schema: 6\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: origin-root\n  socket_path: /run/registered/app-server.sock\n  client_version: 0.172.0\n  binding_schema: 3\n  binding_origin_schema: 1\ncreation:\n  state: ready\n  initial_goal_sent: true\n"
			switch fault {
			case "unstarted-null":
				targets.Members[0].OriginalThreadID, targets.Members[0].ThreadID = nil, nil
				expected = targets.Clone()
			case "legacy-absent":
				delete(marker, "codex_targets")
				expected = nil
			case "wrong-root":
				originalRoot = "other-root"
			case "wrong-capture":
				expected.Revision--
			case "expected-absent":
				expected = nil
			case "capture-absent":
				delete(marker, "codex_targets")
			case "capture-null":
				marker["codex_targets"] = nil
			case "capture-malformed":
				marker["codex_targets"] = map[string]any{"revision": 1}
			case "mixed-pi":
				marker["pi"] = map[string]any{}
			case "phase":
				marker["phase"] = "tracking_preserved"
			case "time":
				marker["removed_at"] = notBefore.Add(-time.Second).Format(time.RFC3339Nano)
			case "operation":
				marker["operation_id"] = strings.Repeat("c", 64)
			case "manifest-root":
				manifest = strings.Replace(manifest, "origin-root", "other-root", 1)
			}
			manifestPath := filepath.Join(directory, "work", ManifestName)
			if fault != "manifest-missing" && fault != "split-candidates" {
				if err := os.WriteFile(manifestPath, []byte(manifest), 0600); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(marker)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "duplicate" {
				data = []byte(strings.Replace(string(data), `"codex_targets":`, `"codex_targets":null,"codex_targets":`, 1))
			}
			path := filepath.Join(directory, "recovery.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if fault == "split-candidates" || fault == "unrelated-incomplete" {
				other := filepath.Join(root, "two-example-candidate")
				if err := os.MkdirAll(other, 0700); err != nil {
					t.Fatal(err)
				}
				marker["recovery"] = other
				if fault == "split-candidates" {
					delete(marker, "codex_targets")
					if err := os.Mkdir(filepath.Join(other, "work"), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(other, "work", ManifestName), []byte(manifest), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					marker["operation_id"], marker["codex_targets"] = strings.Repeat("c", 64), nil
				}
				otherData, _ := json.Marshal(marker)
				if err := os.WriteFile(filepath.Join(other, "recovery.json"), otherData, 0600); err != nil {
					t.Fatal(err)
				}
			}
			completed, err := CompletedRemovalForTarget(workspace, slug, stateRoot, operation, notBefore, originalRoot, expected)
			positive := fault == "none" || fault == "unstarted-null" || fault == "legacy-absent" || fault == "unrelated-incomplete"
			if positive && (err != nil || !completed) || !positive && completed {
				t.Fatal("retained target proof result", fault, completed, err)
			}
			if after, err := os.ReadFile(path); err != nil || string(data) != string(after) {
				t.Fatal("completion proof modified its candidate", err)
			}
			if positive {
				compatible, err := CompletedRemoval(workspace, slug, stateRoot, operation, notBefore)
				if err != nil || !compatible {
					t.Fatal("legacy completion API changed", compatible, err)
				}
				if fault != "unrelated-incomplete" {
					if err := RequireNoPendingRemoval(workspace, slug, stateRoot); err != nil {
						t.Fatal("valid completed candidate remained pending", err)
					}
				}
				if fault == "unstarted-null" {
					marker["state"], marker["phase"] = "prepared", "prepared"
					pending, _ := json.Marshal(marker)
					if err := os.WriteFile(path, pending, 0600); err != nil {
						t.Fatal(err)
					}
					if err := RequireNoPendingRemoval(workspace, slug, stateRoot); err == nil || !strings.Contains(err.Error(), "accepted removal reserves") {
						t.Fatal("null pending owner was not decoded and refused precisely", err)
					}
					if after, err := os.ReadFile(path); err != nil || string(after) != string(pending) {
						t.Fatal("pending null owner was mutated", err)
					}
				}
			}
		})
	}
}

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

func piLifecycleCaptureFixture(t *testing.T, workspace string) PiLifecycleCapture {
	t.Helper()
	b := backendBindingFixture()
	b.Workspace = workspace
	a := backendAuthorityFixture()
	a.Workspace = workspace
	p := PiLifecycleCapture{Kind: "pi", Contract: 1, Binding: b, Authority: a, Runtime: piruntime.RuntimeRef{Session: agentbackend.SessionRef{Workspace: workspace, Slug: b.Slug, Incarnation: b.SessionIncarnation}, InstanceID: "11111111-1111-4111-8111-111111111111", RequestID: "22222222-2222-4222-8222-222222222222", Digest: strings.Repeat("e", 64), Generation: a.Generation}, Members: []piruntime.LifecycleMember{}, Selectors: []PiLifecycleSelector{}}
	for _, name := range []string{"binding", "creation", "completion", "companion", "authority"} {
		p.Selectors = append(p.Selectors, PiLifecycleSelector{Name: name, Path: filepath.Join(workspace, name), SHA256: strings.Repeat("f", 64)})
	}
	settings := piruntime.EffectiveSettings{Provider: "provider", AccountRef: "account", Model: "model", Thinking: "high", PolicyRevision: strings.Repeat("f", 64), Revision: 1}
	for _, bound := range b.Conversations {
		ref := agentbackend.ConversationRef{Session: p.Ref(), Backend: agentbackend.Pi, Member: bound.Address, Incarnation: bound.Incarnation, NativeID: bound.NativeID}
		p.Members = append(p.Members, piruntime.LifecycleMember{Generation: p.Runtime.Generation, Slot: piruntime.LifecycleSlot{State: bound.State, Member: bound.Address, Incarnation: bound.Incarnation, CreationID: b.CreationReceiptID, NativeID: bound.NativeID, InitialSettings: settings}, Conversation: &ref, Settings: &settings, HistoryRef: strings.Repeat("1", 64)})
	}
	if err := p.Validate(workspace, b.Slug); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPiLifecycleMissingCompletionRequiresCreatingRootAndReceipt(t *testing.T) {
	for _, kind := range []string{"creating", "ready", "ready-root", "missing-receipt", "revive"} {
		t.Run(kind, func(t *testing.T) {
			p := piLifecycleCaptureFixture(t, t.TempDir())
			p.Binding.State = "creating"
			p.Authority.State = "starting"
			for index := range p.Binding.Conversations {
				p.Binding.Conversations[index].State = "creating"
				p.Binding.Conversations[index].NativeID = ""
				p.Members[index].Slot.State = "creating"
				p.Members[index].Slot.NativeID = ""
			}
			selectors := []PiLifecycleSelector{}
			for _, selector := range p.Selectors {
				if selector.Name != "completion" {
					selectors = append(selectors, selector)
				}
			}
			p.Selectors = append(selectors, PiLifecycleSelector{Name: "receipt", Path: filepath.Join(p.Binding.Workspace, "receipt"), SHA256: strings.Repeat("f", 64)})
			switch kind {
			case "ready":
				p.Binding.State = "ready"
			case "ready-root":
				p.Binding.Conversations[0].State = "ready"
				p.Binding.Conversations[0].NativeID = p.Members[0].Conversation.NativeID
				p.Members[0].Slot.State = "ready"
				p.Members[0].Slot.NativeID = p.Binding.Conversations[0].NativeID
			case "missing-receipt":
				p.Selectors = p.Selectors[:len(p.Selectors)-1]
			case "revive":
				p.Successor = &piruntime.StartRequest{}
			}
			_, err := DecodePiLifecycleCapture(backendJSON(t, p), p.Binding.Workspace, p.Binding.Slug)
			if (err == nil) != (kind == "creating") {
				t.Fatal(kind, err)
			}
		})
	}
}

func TestPiLifecycleRetainedScopesAndRemovalEpoch(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	p := piLifecycleCaptureFixture(t, workspace)
	operation := strings.Repeat("a", 64)
	journal := map[string]any{"schema": 5, "workspace": workspace, "slug": p.Binding.Slug, "operation_id": operation, "phase": "prepared", "retained_thread_id": nil, "pi": p}
	active := filepath.Join(workspace, "worktrees", ".locks", p.Binding.Slug+".archive.json")
	backendWrite(t, active, backendJSON(t, journal))
	captures, err := InspectRetainedPiLifecycles(context.Background(), root, workspace)
	if err != nil || len(captures) != 1 {
		t.Fatal("active capture", captures, err)
	}
	p.Terminal = &piruntime.HostObservation{LaunchRequest: piruntime.LaunchRequest{InstanceID: p.Runtime.InstanceID, RequestID: p.Runtime.RequestID, Generation: p.Runtime.Generation}, Digest: p.Runtime.Digest, Phase: "terminal", Outcome: "stopped", DrainProof: map[string]any{"empty": true}}
	journal["pi"] = p
	journal["phase"] = "archived"
	archived := filepath.Join(userstate.WorkspaceDirectory(root, "archived-worktrees", workspace), p.Binding.Slug, operation, "lifecycle.json")
	backendWrite(t, archived, backendJSON(t, journal))
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	captures, err = InspectRetainedPiLifecycles(context.Background(), root, workspace)
	if err != nil || len(captures) != 1 || captures[0].Terminal == nil {
		t.Fatal("archived capture", captures, err)
	}
	recovery := filepath.Join(userstate.WorkspaceDirectory(root, "removed", workspace), "20261122-"+p.Binding.Slug+"-123")
	if err := os.MkdirAll(filepath.Join(recovery, "work"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := map[string]any{"schema": 2, "workspace": workspace, "slug": p.Binding.Slug, "operation_id": strings.Repeat("b", 64), "phase": "removed", "state": "removed", "tracking": "work", "recovery": recovery, "removed_at": "2026-11-22T09:00:00Z", "pi": p}
	path := filepath.Join(recovery, "recovery.json")
	backendWrite(t, path, backendJSON(t, marker))
	epoch, err := CompletedRemovalHistory(workspace, p.Binding.Slug, root)
	if err != nil || epoch == fmt.Sprintf("%x", sha256.Sum256(nil)) {
		t.Fatal("Pi deletion epoch missing", epoch, err)
	}
	current := p.Binding
	current.Conversations = append([]BackendConversation{}, p.Binding.Conversations...)
	current.SessionIncarnation = strings.Repeat("9", 64)
	for i := range current.Conversations {
		current.Conversations[i].NativeID = "new-" + current.Conversations[i].NativeID
	}
	selector := filepath.Join(root, "session-backends", fmt.Sprintf("%x", sha256.Sum256([]byte(workspace))), p.Binding.Slug+".json")
	backendWrite(t, selector, backendJSON(t, current))
	captures, err = InspectRetainedPiLifecycles(context.Background(), root, workspace)
	if err != nil || len(captures) != 2 {
		t.Fatal("retired scopes", captures, err)
	}
	for _, capture := range captures {
		if capture.Ref() != p.Ref() || capture.Binding.Conversations[0].NativeID != p.Binding.Conversations[0].NativeID {
			t.Fatal("fresh same slug replaced old audit", capture)
		}
	}
	p.Terminal.DrainProof["empty"] = false
	marker["pi"] = p
	backendWrite(t, path, backendJSON(t, marker))
	if _, err := CompletedRemovalHistory(workspace, p.Binding.Slug, root); err == nil {
		t.Fatal("nonempty Pi removal drain accepted")
	}
}

func TestRequireNoPendingRemovalUsesExistingPrivateCandidateOwner(t *testing.T) {
	for _, fault := range []string{"absent", "empty", "terminal", "captured-terminal", "pending-before-move", "malformed", "wrong-workspace", "wrong-slug", "wrong-recovery", "nonempty-no-marker", "unsafe-root", "unsafe-candidate", "duplicate", "ambiguous", "terminal-missing-tracking", "terminal-time", "capture-null", "capture-malformed"} {
		t.Run(fault, func(t *testing.T) {
			workspace, stateRoot := t.TempDir(), t.TempDir()
			slug := "example"
			root := userstate.WorkspaceDirectory(stateRoot, "removed", workspace)
			directory := filepath.Join(root, "one-example-candidate")
			if fault == "absent" {
				if err := RequireNoPendingRemoval(workspace, slug, stateRoot); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			if fault == "empty" {
				if err := RequireNoPendingRemoval(workspace, slug, stateRoot); err != nil {
					t.Fatal(err)
				}
				return
			}
			marker := map[string]any{"schema": 1, "slug": slug, "workspace": workspace, "operation_id": strings.Repeat("a", 64), "state": "prepared", "phase": "prepared", "tracking": "work", "recovery": directory}
			if strings.Contains(fault, "terminal") {
				marker["state"], marker["phase"], marker["removed_at"] = "removed", "removed", time.Now().UTC().Format(time.RFC3339Nano)
				if fault != "terminal-missing-tracking" {
					if err := os.Mkdir(filepath.Join(directory, "work"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch fault {
			case "wrong-workspace":
				marker["workspace"] = workspace + "-other"
			case "wrong-slug":
				marker["slug"] = "foreign"
			case "wrong-recovery":
				marker["recovery"] = directory + "-other"
			case "terminal-time":
				marker["removed_at"] = "invalid"
			case "capture-null":
				marker["codex_targets"] = nil
			case "capture-malformed":
				marker["codex_targets"] = map[string]any{}
			case "captured-terminal":
				marker["codex_targets"] = &CodexTargets{Incarnation: strings.Repeat("b", 64), Revision: 1, ThreadID: "native-root", Members: []CodexTargetMember{}}
			}
			data, _ := json.Marshal(marker)
			if fault == "malformed" {
				data = []byte("{")
			}
			if fault == "duplicate" {
				data = []byte(strings.Replace(string(data), `"schema":1`, `"schema":1,"schema":1`, 1))
			}
			path := filepath.Join(directory, "recovery.json")
			if fault == "nonempty-no-marker" {
				path = filepath.Join(directory, "unsealed-file")
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if fault == "unsafe-root" {
				if err := os.Chmod(root, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "unsafe-candidate" {
				if err := os.Chmod(directory, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "ambiguous" {
				other := filepath.Join(root, "two-example-candidate")
				if err := os.Mkdir(other, 0700); err != nil {
					t.Fatal(err)
				}
				marker["recovery"] = other
				otherData, _ := json.Marshal(marker)
				if err := os.WriteFile(filepath.Join(other, "recovery.json"), otherData, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := RequireNoPendingRemoval(workspace, slug, stateRoot)
			positive := fault == "terminal" || fault == "captured-terminal"
			if positive && err != nil || !positive && err == nil {
				t.Fatal("pending removal guard", fault, err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || string(after) != string(data) {
				t.Fatal("read-only guard changed candidate", readErr)
			}
			if positive {
				completed, err := CompletedRemoval(workspace, slug, stateRoot, strings.Repeat("a", 64), time.Time{})
				if err != nil || !completed {
					t.Fatal("compatibility reader changed", completed, err)
				}
			}
		})
	}
}
