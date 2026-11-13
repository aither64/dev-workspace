package piruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func retainedPathModes(t *testing.T, root string) map[string]os.FileMode {
	t.Helper()
	modes := map[string]os.FileMode{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err == nil {
			modes[path] = info.Mode()
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return modes
}

func TestInspectRetainedConversationsGenericRootRefusals(t *testing.T) {
	for _, kind := range []string{"group-write", "world-write", "owner-access", "symlink", "ancestor-symlink", "noncanonical", "missing", "not-directory", "lookup-not-directory", "lookup-access", "foreign", "private-mode", "private-symlink", "canceled", "workspace", "duplicate-workspace"} {
		t.Run(kind, func(t *testing.T) {
			state, workspace := t.TempDir(), t.TempDir()
			if err := os.Chmod(state, 0755); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			workspaces := []string{workspace}
			switch kind {
			case "group-write", "world-write", "owner-access":
				mode := map[string]os.FileMode{"group-write": 0770, "world-write": 0702, "owner-access": 0600}[kind]
				if err := os.Chmod(state, mode); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(state, 0700) })
			case "symlink", "ancestor-symlink":
				alias := filepath.Join(t.TempDir(), "alias")
				target := state
				if kind == "ancestor-symlink" {
					target = filepath.Dir(state)
				}
				if err := os.Symlink(target, alias); err != nil {
					t.Fatal(err)
				}
				if kind == "ancestor-symlink" {
					alias = filepath.Join(alias, filepath.Base(state))
				}
				state = alias
			case "noncanonical":
				state += "/../" + filepath.Base(state)
			case "missing":
				state = filepath.Join(state, "missing")
			case "not-directory", "lookup-not-directory":
				state = filepath.Join(state, "file")
				if err := os.WriteFile(state, nil, 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "lookup-not-directory" {
					state = filepath.Join(state, "child")
				}
			case "foreign":
				state = "/"
				info, err := os.Lstat(state)
				if err != nil || info.Sys().(*syscall.Stat_t).Uid == uint32(os.Geteuid()) {
					t.Fatal("foreign root fixture requires the declared unprivileged environment", err)
				}
			case "lookup-access":
				parent := state
				state = filepath.Join(parent, "state")
				if err := os.Mkdir(state, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(parent, 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
			case "private-mode", "private-symlink":
				path := filepath.Join(state, "pi-runtime")
				if kind == "private-mode" {
					if err := os.Mkdir(path, 0755); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "workspace":
				workspaces = []string{"relative"}
			case "duplicate-workspace":
				workspaces = append(workspaces, workspace)
			}
			rows, err := InspectRetainedConversations(ctx, state, workspaces)
			if err == nil || rows != nil || kind == "canceled" && !errors.Is(err, context.Canceled) || strings.HasPrefix(kind, "lookup-") && os.IsNotExist(err) {
				t.Fatal("unsafe/invalid generic scope accepted or misclassified", rows, err)
			}
		})
	}
}

func retainedMappingFixture(t *testing.T) (string, string, string, conversationRecord) {
	t.Helper()
	state, workspace := t.TempDir(), t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	ref := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: workspace, Slug: "2026-11-12-fixture", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "11111111-1111-4111-8111-111111111111"}
	dir, err := stateDirectory(state, ref.Session)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "members", ref.Member, ref.Incarnation)
	r := conversationRecord{Schema: 1, Conversation: ref, CreationID: strings.Repeat("d", 64), HistoryRef: historyReference(ref), Generation: strings.Repeat("c", 64), Settings: settingsFixture(), InitialSettings: settingsFixture()}
	if err := writePrivate(filepath.Join(root, "conversation.json"), r); err != nil {
		t.Fatal(err)
	}
	return state, workspace, root, r
}

func TestInspectRetainedConversationsKeepsColdHeldPendingAndRefusesSchema3(t *testing.T) {
	for _, kind := range []string{"legacy", "cold", "held", "pending", "loaded"} {
		t.Run(kind, func(t *testing.T) {
			state, workspace, root, r := retainedMappingFixture(t)
			if kind != "legacy" {
				r.Schema = 2
				p := ToolPolicyTarget{Mode: "plan", Revision: 1, Active: []string{"read"}, RegisteredCeiling: []string{"read"}}
				p.Digest = ToolPolicyDigest(p)
				r.ToolPolicy = &p
				if kind == "pending" {
					next := p
					next.Revision = 2
					r.PendingToolPolicy = &next
				}
				if kind == "held" {
					r.HeldReason = "retained uncertainty"
				}
				r.Loaded = kind == "loaded"
			} else {
				r.CreationID = "22222222-2222-4222-8222-222222222222"
			}
			if err := writePrivate(filepath.Join(root, "conversation.json"), r); err != nil {
				t.Fatal(err)
			}
			before := privateInventory(t, state)
			rows, err := InspectRetainedConversations(context.Background(), state, []string{workspace})
			if err != nil || len(rows) != 1 || rows[0].Conversation != r.Conversation || rows[0].CreationID != r.CreationID || rows[0].Schema != r.Schema {
				t.Fatal(rows, err)
			}
			if !reflect.DeepEqual(before, privateInventory(t, state)) {
				t.Fatal("inventory wrote state")
			}
		})
	}
	for _, kind := range []string{"schema3", "unknown", "scope", "member", "creation", "mode", "temp", "missing", "symlink", "owner", "file-mode", "oversized", "duplicate", "null", "history-path", "foreign-workspace"} {
		t.Run(kind, func(t *testing.T) {
			state, workspace, root, r := retainedMappingFixture(t)
			path := filepath.Join(root, "conversation.json")
			switch kind {
			case "schema3":
				r.Schema = 3
			case "unknown":
				r.Schema = 4
			case "scope":
				r.Conversation.Session.Incarnation = strings.Repeat("e", 64)
			case "member":
				r.Conversation.Member = "implementer0"
			case "creation":
				r.CreationID = "changed"
			case "owner":
				r.OwnerKind = "utility"
			case "history-path":
				r.HistoryFile = filepath.Join(root, "outside.jsonl")
			}
			if err := writePrivate(path, r); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "foreign-workspace":
				workspace = t.TempDir()
			case "file-mode":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(path, []byte(strings.Repeat(" ", recordLimit+1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate", "null":
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				replacement := `"Schema":1,"Schema":1`
				if kind == "null" {
					replacement = `"Schema":null`
				}
				if err := os.WriteFile(path, []byte(strings.Replace(string(data), `"Schema":1`, replacement, 1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(root, 0755); err != nil {
					t.Fatal(err)
				}
			case "temp":
				if err := os.WriteFile(filepath.Join(root, ".pi-record-lost"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-original", path); err != nil {
					t.Fatal(err)
				}
			}
			if rows, err := InspectRetainedConversations(context.Background(), state, []string{workspace}); err == nil || rows != nil {
				t.Fatal("invalid mapping accepted", kind, rows, err)
			}
		})
	}
}
