package communication

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type nativeFixture struct {
	NativeClient
	metadata codex.ThreadMetadata
	activity codex.ActivitySnapshot
}

func (f *nativeFixture) ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error) {
	return f.metadata, nil
}
func (f *nativeFixture) ReadActivity(context.Context, string) (codex.ActivitySnapshot, error) {
	return f.activity, nil
}

func TestNativePreservesRecipientSettingsAndHolds(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	authority := filepath.Join(root, "authority")
	if err := os.Mkdir(authority, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	id := Identity{"receiver", "receiver-thread", "identity"}
	client := &nativeFixture{metadata: codex.ThreadMetadata{ID: id.Thread, Cwd: filepath.Join(root, "work", id.Slug)}, activity: codex.ActivitySnapshot{CurrentState: "idle"}}
	n := Native{Workspace: root, StateRoot: root, AuthorityDir: authority, Socket: socket, Client: client}
	a := session.RuntimeAuthority{Schema: 1, State: "ready", Slug: id.Slug, Workspace: root, TmuxSocket: filepath.Join(root, "tmux"), TmuxSessionID: "$1", CodexThreadID: id.Thread, CodexSocketPath: socket, CodexClientVersion: "0.160.0"}
	data, _ := json.Marshal(a)
	if err := os.WriteFile(filepath.Join(authority, id.Slug+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	recovery := session.RecoveryStore{Root: root, Workspace: root}
	if err := recovery.Set(ctx, id.Slug, id.Thread, socket, true, true); err != nil {
		t.Fatal(err)
	}
	for _, activity := range []codex.ActivitySnapshot{{CurrentState: "idle"}, {CurrentState: "working"}, {CurrentState: "waiting", WaitReason: "sleep"}} {
		client.activity = activity
		if reason, err := n.Ready(ctx, id); reason != "" || err != nil {
			t.Fatalf("eligible recipient held: %s %v", reason, err)
		}
	}
	for _, reason := range []string{"approval", "userInput"} {
		client.activity = codex.ActivitySnapshot{CurrentState: "waiting", WaitReason: reason}
		if held, _ := n.Ready(ctx, id); held == "" {
			t.Fatalf("%s was not held", reason)
		}
	}
	client.activity = codex.ActivitySnapshot{CurrentState: "idle"}
	if err := recovery.Set(ctx, id.Slug, id.Thread, socket, false, false); err != nil {
		t.Fatal(err)
	}
	if held, _ := n.Ready(ctx, id); held == "" {
		t.Fatal("stopped recipient activated")
	}
	store, err := teamruntime.NewStore(root, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(ctx, id.Slug, id.Thread, true, func(r *teamruntime.Roster) error {
		r.PresetID = "solo"
		r.CatalogDigest = strings.Repeat("a", 64)
		r.TeamDigest = strings.Repeat("b", 64)
		r.LeadModel = "retained-model"
		r.LeadEffort = "xhigh"
		r.LeadInstructions = "Keep the recipient task constraints."
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	options, err := n.Prepare(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if options.Model != "" || options.ReasoningEffort != "" {
		t.Fatal("delivery overrides retained settings")
	}
	if !strings.Contains(options.ThreadPolicy.DeveloperInstructions, "Keep the recipient task constraints.") {
		t.Fatal("delivery dropped retained instructions")
	}
}
