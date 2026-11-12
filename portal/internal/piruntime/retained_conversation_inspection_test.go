package piruntime

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

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
