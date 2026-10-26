package web

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

// Retain the actual portal conversation admission, as an in-flight queue
// reconciliation does. The creator must contend with this same inode, not an
// unrelated fixture mutex or a guessed lock pathname.
func holdCreationConversation(t *testing.T, s *Server, slug string) func() {
	t.Helper()
	target, err := s.resolveConversation(context.Background(), conversation.ResolveRequest{ID: slug, Operation: "queue/reconcile", Mutation: true})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(target.Release) }
	t.Cleanup(release)
	if lock, err := session.LockRuntimeExclusive(s.config.AuthorityDir, slug); err == nil {
		lock.Close()
		t.Fatal("conversation admission did not retain its shared runtime lock")
	}
	var stat unix.Stat_t
	if err := unix.Stat(filepath.Join(s.config.AuthorityDir, slug+".lock"), &stat); err != nil {
		t.Fatal(err)
	}
	t.Logf("holder=portal queue/reconcile mode=SH lock=%d:%d", stat.Dev, stat.Ino)
	return release
}

func TestSessionPreparationOccupiedConversationDoesNotBlockNameSelection(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprintf("custom=%t", custom), func(t *testing.T) {
			s := newTestServer(t)
			const occupied = "2026-10-03-development-session"
			prepareInteractiveConversation(t, s, occupied)
			s.config.Codex = &browserContractCodex{transcript: codex.Transcript{ThreadID: "thread-1", Status: "idle"}}
			// This collision fixture starts with a successful generated name;
			// naming backend failures have their own refusal coverage.
			var namingCalls atomic.Int32
			s.config.SessionNamer = func(context.Context, string, string) (string, error) {
				namingCalls.Add(1)
				return `{"name":"development-session"}`, nil
			}
			release := holdCreationConversation(t, s, occupied)
			defer release()
			name := ""
			if custom {
				name = "development-session"
			}
			id := preparationTestID(290)
			if response := postPreparation(t, s, id, "Preserve this accepted request", name, nil); response.Code != http.StatusAccepted {
				t.Fatal(response.Code, response.Body.String())
			}
			result := awaitPreparation(t, s, id)
			if custom {
				if result.State != "failed" || result.Slug != "" || result.Error != errPreparationOccupied.Error() {
					t.Fatalf("custom collision changed meaning: %+v", result)
				}
				if namingCalls.Load() != 0 {
					t.Fatal("custom collision called automatic naming")
				}
				return
			}
			if result.Slug != occupied+"-2" || result.Handoff == nil || result.Attempt != 1 || result.Error != "" {
				t.Fatalf("occupied conversation blocked generated-name suffix selection: %+v", result)
			}
			if result.Base != "development-session" || result.NamingOutcome != "model" || namingCalls.Load() != 1 {
				t.Fatalf("collision did not retain one generated name: %+v calls=%d", result, namingCalls.Load())
			}
			if result.Snapshot.Input.RawPrompt != "Preserve this accepted request" {
				t.Fatal("collision changed the accepted request")
			}
			before := result.ReceiptID
			if response := postPreparation(t, s, id, "Preserve this accepted request", name, nil); response.Code != http.StatusAccepted {
				t.Fatal(response.Code)
			}
			if replay := awaitPreparation(t, s, id); replay.ReceiptID != before || replay.Slug != result.Slug || replay.Attempt != result.Attempt {
				t.Fatal("collision replay changed the reservation")
			}
			if namingCalls.Load() != 1 {
				t.Fatal("collision replay called automatic naming again")
			}
		})
	}
}
