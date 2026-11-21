package communication

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
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

type nativeProofFixture struct {
	NativeClient
	attempted, prepared bool
	err                 error
	cancel              context.CancelFunc
	calls               []string
	message             Message
}

func (f *nativeProofFixture) SendAttemptedWithOptionsResumable(_ context.Context, thread, text, id, action string, options codex.TurnOptions) (bool, error) {
	f.calls = append(f.calls, "history")
	if thread != f.message.To.Thread || text != f.message.Envelope || id != f.message.ID || action != "communication:"+f.message.GrantID || !reflect.DeepEqual(options, *f.message.Options) {
		return false, errors.New("changed original")
	}
	if f.cancel != nil {
		f.cancel()
	}
	return f.attempted, f.err
}
func (f *nativeProofFixture) IsPreparedSendWithOptions(thread, text, id, action string, options codex.TurnOptions) (bool, error) {
	f.calls = append(f.calls, "prepared")
	if thread != f.message.To.Thread || text != f.message.Envelope || id != f.message.ID || action != "communication:"+f.message.GrantID || !reflect.DeepEqual(options, *f.message.Options) {
		return false, errors.New("changed prepared original")
	}
	return f.prepared, f.err
}
func TestNativeOriginalSubmissionRequiresPositiveProof(t *testing.T) {
	m := Message{ID: strings.Repeat("a", 32), GrantID: strings.Repeat("b", 32), To: Identity{Thread: "original-thread"}, Envelope: "exact envelope", Options: &codex.TurnOptions{Model: "original-model"}}
	for _, tc := range []struct {
		name                      string
		attempted, prepared, want bool
		err                       error
		cancel                    bool
	}{
		{name: "complete-negative", want: true}, {name: "exact-prepared", attempted: true, prepared: true, want: true},
		{name: "native-attempt", attempted: true}, {name: "incomplete", err: context.DeadlineExceeded}, {name: "canceled", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &nativeProofFixture{message: m, attempted: tc.attempted, prepared: tc.prepared, err: tc.err}
			if tc.cancel {
				f.cancel = cancel
			}
			got, err := (Native{Client: f}).CanSubmitOriginal(ctx, m)
			if got != tc.want || tc.want && err != nil {
				t.Fatal(got, err)
			}
			if (tc.err != nil || tc.cancel) && err == nil {
				t.Fatal("incomplete proof lost its error")
			}
			if !tc.attempted && len(f.calls) != 1 {
				t.Fatal(f.calls)
			}
		})
	}
	if ok, err := (Native{Client: &nativeFixture{}}).CanSubmitOriginal(context.Background(), m); ok || err == nil {
		t.Fatal("old client supplied proof")
	}
}
