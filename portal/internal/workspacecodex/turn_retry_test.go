package workspacecodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/coder/websocket"
)

func retryProofFixture() (turnretry.Scope, *turnretry.Binding, codex.TurnContext, turnretry.Snapshot) {
	scope := turnretry.Scope{Workspace: "/workspace", Slug: "example", Root: "root", Thread: "member", Socket: "/socket"}
	b := &turnretry.Binding{Epoch: 1, SocketIdentity: "epoch", Provider: "selected", CommonInstructions: "common", Policy: codex.ThreadPolicy{DeveloperInstructions: "retained role", Sandbox: "danger-full-access", ApprovalPolicy: "never"}}
	b.Digest, _ = b.Hash()
	native := codex.TurnContext{ThreadID: "member", TurnID: "executed", Cwd: "/workspace/work/example", Model: "actual-model", ReasoningEffort: "xhigh", CollaborationMode: "plan", ApprovalPolicy: json.RawMessage(`"never"`), SandboxPolicy: json.RawMessage(`{"type":"danger-full-access"}`), ApprovalsReviewer: "user"}
	p := turnretry.Snapshot{Epoch: 1, Version: 1, ID: "queue-id", Origin: "queue", TextDigest: textHash("saved input"), Provider: b.Provider, SocketIdentity: b.SocketIdentity, CommonInstructions: b.CommonInstructions, BindingDigest: b.Digest, Options: codex.TurnOptions{Model: "enqueue-model", ReasoningEffort: "low", ThreadPolicy: b.Policy}}
	p.Digest, _ = p.Hash()
	return scope, b, native, p
}

func TestRetryManagedSettingsIntentCancelsBeforeNativeMutation(t *testing.T) {
	model, effort, mode := "changed", "high", "plan"
	for name, update := range map[string]codex.ThreadSettingsUpdate{
		"model": {Model: &model}, "effort": {ReasoningEffort: &effort}, "mode": {CollaborationMode: &mode},
	} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			cwd := filepath.Join(workspace, "work", "example")
			if err := os.MkdirAll(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "state.md"), []byte("---\nlifecycle: active\n---\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "plan.md"), []byte("# Synthetic settings test\n"), 0600); err != nil {
				t.Fatal(err)
			}
			socket := serveUnixWebsocket(t, func(conn *websocket.Conn) error {
				if err := handshake(conn); err != nil {
					return err
				}
				for {
					req, err := readObject(conn)
					if err != nil {
						return nil
					}
					if req["method"] == "thread/turns/list" {
						if err := writeObject(conn, map[string]any{"id": req["id"], "result": map[string]any{"data": []any{map[string]any{"id": "failed", "status": "failed"}}}}); err != nil {
							return nil
						}
						continue
					}
					if req["method"] != "thread/read" { // A failed settings RPC must still suppress the pending retry.
						return writeObject(conn, map[string]any{"id": req["id"], "error": map[string]any{"code": -32000, "message": "synthetic settings mutation rejected"}})
					}
					if err := writeObject(conn, map[string]any{"id": req["id"], "result": map[string]any{"thread": map[string]any{"id": "root", "cwd": cwd, "source": "vscode", "status": map[string]any{"type": "idle"}}}}); err != nil {
						return nil
					}
				}
			})
			body := fmt.Sprintf("schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: root\n  socket_path: %s\n  client_version: 0.163.0\n", socket)
			if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			client := NewWithOptions(socket, workspace, codex.ClientOptions{})
			defer client.Close()
			client.RecoveryRoot = t.TempDir()
			client.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return codex.ThreadPolicy{}, nil }
			scope := turnretry.Scope{Workspace: workspace, Slug: "example", Root: "root", Thread: "root", Socket: socket}
			tx, err := client.RetryStore().Lock(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, proof := retryProofFixture()
			proof.Turn, proof.Proven, proof.Mode = "failed", true, "default"
			proof.Digest, _ = proof.Hash()
			tx.State.Enroll(turnretry.Observation{})
			tx.State.AddSnapshot(proof)
			if err := tx.State.Observe(turnretry.Observation{Turn: "failed", Status: "failed", Code: "serverOverloaded"}, time.Now(), 0, turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
			tx.Close()
			_, updateErr := client.UpdateThreadSettings(context.Background(), "root", update)
			saved, err := client.RetryStore().Load(scope)
			if err != nil || saved.Pending != nil || saved.Phase != "cancelled" {
				t.Fatal("settings intent retained automatic work", saved, err, updateErr)
			}
		})
	}
}

func TestRetryQueueProofUsesActualExecutionSettingsAndRetainedBinding(t *testing.T) {
	scope, b, native, p := retryProofFixture()
	actual, err := proveContributors(scope, b, native, []turnretry.Snapshot{p})
	if err != nil {
		t.Fatal(err)
	}
	if !actual.Proven || actual.Options.Model != "actual-model" || actual.Options.ReasoningEffort != "xhigh" || actual.Mode != "plan" || actual.Turn != "executed" {
		t.Fatalf("inferred enqueue settings: %+v", actual)
	}
	if actual.Options.ThreadPolicy.DeveloperInstructions != "retained role" || actual.CommonInstructions != "common" || actual.Provider != "selected" {
		t.Fatal("lost non-native binding")
	}
	if len(actual.NativeContext) == 0 || actual.Digest == p.Digest {
		t.Fatal("execution context not bound")
	}
}

func TestRetryProofRejectsChangedAuthorityAndContributingContext(t *testing.T) {
	for _, change := range []func(*turnretry.Binding, *codex.TurnContext, *turnretry.Snapshot){
		func(b *turnretry.Binding, _ *codex.TurnContext, _ *turnretry.Snapshot) { b.Provider = "changed" },
		func(b *turnretry.Binding, _ *codex.TurnContext, _ *turnretry.Snapshot) {
			b.SocketIdentity = "restarted"
		},
		func(_ *turnretry.Binding, n *codex.TurnContext, _ *turnretry.Snapshot) { n.ThreadID = "native-child" },
		func(_ *turnretry.Binding, n *codex.TurnContext, _ *turnretry.Snapshot) {
			n.SandboxPolicy = json.RawMessage(`{"type":"workspace-write"}`)
		},
		func(_ *turnretry.Binding, n *codex.TurnContext, _ *turnretry.Snapshot) {
			n.ApprovalPolicy = json.RawMessage(`"on-request"`)
		},
		func(_ *turnretry.Binding, _ *codex.TurnContext, p *turnretry.Snapshot) {
			p.Options.ThreadPolicy.DeveloperInstructions = "different"
		},
		func(_ *turnretry.Binding, _ *codex.TurnContext, p *turnretry.Snapshot) {
			p.Options.AdditionalContext = map[string]codex.AdditionalContextEntry{"unknown": {Kind: "application", Value: "foreign"}}
		},
	} {
		scope, b, n, p := retryProofFixture()
		change(b, &n, &p)
		if _, err := proveContributors(scope, b, n, []turnretry.Snapshot{p}); !errors.Is(err, turnretry.ErrMissingProof) {
			t.Fatal("changed binding accepted", err)
		}
	}
}

func TestRetrySteerReceiptRetainsValidAdmissionAcrossRestart(t *testing.T) {
	scope, _, _, p := retryProofFixture()
	store := turnretry.Store{Root: t.TempDir(), Workspace: scope.Workspace}
	tx, err := store.Lock(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	p.Origin = "send"
	p.Digest, _ = p.Hash()
	tx.State.AddSnapshot(p)
	err = bindRetryReceipt(tx, p.ID, codex.SendReceipt{TurnID: "active", Steered: true})
	tx.Close()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Load(scope)
	if err != nil {
		t.Fatal(err)
	}
	got := saved.Snapshots[0]
	if got.Origin != "steer" || got.Turn != "active" || got.Digest == p.Digest {
		t.Fatal("steered admission lost its exact receipt", got)
	}
}

func TestRetrySteerInheritsNativeTurnSettingsAndAccountsForAllContext(t *testing.T) {
	scope, b, n, p := retryProofFixture()
	p.Origin = "send"
	p.Options.AdditionalContext = map[string]codex.AdditionalContextEntry{"first": {Kind: "application", Value: "initial context"}}
	steer := p
	steer.ID = "steer-id"
	steer.Origin = "steer"
	steer.Options.Model = "requested-other-model"
	steer.Options.ReasoningEffort = "low"
	steer.Options.AdditionalContext = map[string]codex.AdditionalContextEntry{"second": {Kind: "application", Value: "followup context"}}
	actual, err := proveContributors(scope, b, n, []turnretry.Snapshot{p, steer})
	if err != nil {
		t.Fatal(err)
	}
	if actual.Options.Model != n.Model || actual.Options.ReasoningEffort != n.ReasoningEffort || len(actual.Options.AdditionalContext) != 2 {
		t.Fatal("steer replaced original settings or context")
	}
	steer.Options.AdditionalContext = map[string]codex.AdditionalContextEntry{"first": {Kind: "application", Value: "conflicting"}}
	if _, err := proveContributors(scope, b, n, []turnretry.Snapshot{p, steer}); err == nil {
		t.Fatal("ambiguous context accepted")
	}
}

func TestRetrySettingsEpochFencesUnscheduledAndQueuedExecution(t *testing.T) {
	model, effort, plan, normal := "new-model", "high", "plan", "default"
	updates := map[string]codex.ThreadSettingsUpdate{"model": {Model: &model}, "effort": {ReasoningEffort: &effort}, "default-to-plan": {CollaborationMode: &plan}, "plan-to-default": {CollaborationMode: &normal}}
	for name, update := range updates {
		for _, situation := range []string{"active", "failed-unobserved", "queue-race"} {
			t.Run(name+"/"+situation, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				workspace := t.TempDir()
				cwd := filepath.Join(workspace, "work", "example")
				if err := os.MkdirAll(cwd, 0700); err != nil {
					t.Fatal(err)
				}
				current := "manual"
				status := "failed"
				if situation != "failed-unobserved" {
					status = "inProgress"
				}
				var client *Client
				var scope turnretry.Scope
				socket := serveUnixWebsocket(t, func(conn *websocket.Conn) error {
					if err := handshake(conn); err != nil {
						return err
					}
					for {
						req, err := readObject(conn)
						if err != nil {
							return nil
						}
						var result any
						switch req["method"] {
						case "thread/read":
							result = map[string]any{"thread": map[string]any{"id": "root", "cwd": cwd, "source": "vscode", "modelProvider": "fixture", "model": "new-model", "reasoningEffort": "high", "status": map[string]any{"type": "idle"}}}
						case "thread/turns/list":
							result = map[string]any{"data": []any{map[string]any{"id": current, "status": status}}}
						default:
							saved, err := client.RetryStore().Load(scope)
							if err != nil || saved.Epoch != 2 {
								t.Errorf("settings effect preceded durable epoch: %v %v", saved, err)
							}
							// Native queue turnover after the identity read cannot acquire the new epoch.
							if situation == "queue-race" {
								current, status = "queued-before-settings", "failed"
							}
							if err := writeObject(conn, map[string]any{"id": req["id"], "error": map[string]any{"code": -32000, "message": "synthetic failed settings response"}}); err != nil {
								return nil
							}
							continue
						}
						if err := writeObject(conn, map[string]any{"id": req["id"], "result": result}); err != nil {
							return nil
						}
					}
				})
				for path, body := range map[string]string{"state.md": "---\nlifecycle: active\n---\n", "plan.md": "# settings boundary\n", "portal.yml": "schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: root\n  socket_path: " + socket + "\n  client_version: 0.163.0\n"} {
					if err := os.WriteFile(filepath.Join(cwd, path), []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
				client = NewWithOptions(socket, workspace, codex.ClientOptions{})
				defer client.Close()
				client.RecoveryRoot = t.TempDir()
				client.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return codex.ThreadPolicy{}, nil }
				scope = turnretry.Scope{Workspace: workspace, Slug: "example", Root: "root", Thread: "root", Socket: socket}
				tx, err := client.RetryStore().Lock(ctx, scope)
				if err != nil {
					t.Fatal(err)
				}
				_, _, native, old := retryProofFixture()
				old.Options.ThreadPolicy = codex.ThreadPolicy{}
				old.CommonInstructions = ""
				old.Provider = "fixture"
				old.BindingDigest = ""
				old.SocketIdentity, _ = session.SocketIdentity(socket)
				old.Turn = "manual"
				old.Origin = "queue"
				old.Digest, _ = old.Hash()
				tx.State.Enroll(turnretry.Observation{})
				tx.State.AddSnapshot(old)
				quotaID, _ := turnretry.NewID()
				tx.State.Cyber = append(tx.State.Cyber, turnretry.Charge{ID: quotaID, AtMS: time.Now().UnixMilli(), Accepted: true})
				if err := tx.Save(); err != nil {
					t.Fatal(err)
				}
				tx.Close()
				if _, err := client.UpdateThreadSettings(ctx, "root", update); err == nil {
					t.Fatal("fixture settings unexpectedly succeeded")
				}
				saved, err := client.RetryStore().Load(scope)
				if err != nil {
					t.Fatal(err)
				}
				if saved.Epoch != 2 || saved.Handled != "manual" || saved.Pending != nil || !saved.Enabled || !saved.Enrolled || len(saved.Cyber) != 1 {
					t.Fatal("unscheduled execution survived settings boundary", saved)
				}
				// A new reader must retain the barrier even after the native RPC failed.
				if err := client.RetryBackend().Check(ctx, scope, old); !errors.Is(err, turnretry.ErrMissingProof) {
					t.Fatal("old proof passed current dispatch", err)
				}
				tx, err = client.RetryStore().Lock(ctx, scope)
				if err != nil {
					t.Fatal(err)
				}
				if err := client.captureRetry(ctx, tx, codex.ThreadPolicy{}, "new independently queued task", "new-queue", "queue", codex.TurnOptions{Model: model, ReasoningEffort: effort}); err != nil {
					t.Fatal(err)
				}
				if err := client.recordRetryBinding(ctx, tx, codex.ThreadPolicy{}); err != nil {
					t.Fatal(err)
				}
				binding := tx.State.Binding
				var fresh turnretry.Snapshot
				for _, p := range tx.State.Snapshots {
					if p.ID == old.ID && p.BindingDigest != "" {
						t.Fatal("stale empty snapshot retrobound")
					}
					if p.ID == "new-queue" {
						fresh = p
					}
				}
				if binding == nil || binding.Epoch != 2 || fresh.Epoch != 2 || fresh.BindingDigest != binding.Digest {
					t.Fatal("new admission lost current authority")
				}
				native.ThreadID, native.Cwd, native.TurnID = "root", cwd, "queued-before-settings"
				native.Model, native.ReasoningEffort = model, effort
				if _, err := proveContributors(scope, binding, native, []turnretry.Snapshot{old}); !errors.Is(err, turnretry.ErrMissingProof) {
					t.Fatal("autoqueue race acquired new authority", err)
				}
				if _, err := proveContributors(scope, binding, native, []turnretry.Snapshot{fresh}); err != nil {
					t.Fatal("new managed queue cannot prove actual settings", err)
				}
				tx.Close()
			})
		}
	}
}
