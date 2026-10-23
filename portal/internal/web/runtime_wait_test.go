package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func TestHTTPMutationWaitsForRuntimeExclusiveOwner(t *testing.T) {
	for _, route := range []string{"plan", "message", "queue", "queue/reconcile", "member-message", "member-queue", "member-queue/reconcile"} {
		for _, outcome := range []string{"released", "deadline", "generation", "authority", "lifecycle", "cold"} {
			if route != "plan" && outcome != "released" && outcome != "deadline" {
				continue
			}
			t.Run(route+"/"+outcome, func(t *testing.T) {
				f := newPlanDeliveryFixture(t)
				var cold atomic.Bool
				verify := f.s.config.VerifyThread
				f.s.config.VerifyThread = func(ctx context.Context, thread, cwd string) error {
					if cold.Load() {
						return fmt.Errorf("synthetic cold execution hold")
					}
					return verify(ctx, thread, cwd)
				}
				id := "example"
				if strings.HasPrefix(route, "member-") {
					service, err := f.s.teamService("example")
					if err != nil {
						t.Fatal(err)
					}
					member := teamruntime.Member{Address: "implementer0", Role: "implementer", Thread: "member-thread", Model: "gpt-6.1-sol", Effort: "xhigh", Behavior: "implementer", Access: "workspace_write", State: "ready", AddedAt: time.Now().UTC()}
					if _, err := service.Store.Update(context.Background(), "example", "thread-1", true, func(r *teamruntime.Roster) error { r.Members = []teamruntime.Member{member}; return nil }); err != nil {
						t.Fatal(err)
					}
					f.client.RetryPolicyResolver = func(ctx context.Context, thread string) (policy codex.ThreadPolicy, err error) {
						if thread == member.Thread {
							return service.MemberTurnPolicy("example", "thread-1", member)
						}
						return policy, nil
					}
					id = teamConversationID("example", "implementer0")
					if err := (session.RecoveryStore{Root: f.s.config.UserStateRoot, Workspace: f.s.config.Workspace}).Update(context.Background(), "example", func(r *session.Recovery) error {
						r.ActiveThreads = append(r.ActiveThreads, "member-thread")
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				exclusive, err := session.LockRuntimeExclusive(f.s.config.AuthorityDir, "example")
				if err != nil {
					t.Fatal(err)
				}
				defer exclusive.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				var deadlineCancel context.CancelFunc
				if outcome == "deadline" {
					ctx, deadlineCancel = context.WithTimeout(ctx, 60*time.Millisecond)
					defer deadlineCancel()
				}
				started := make(chan struct{})
				result := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					close(started)
					if route == "plan" {
						result <- f.request(ctx, planDeliveryID, "plan-turn", "same")
						return
					}
					operation := strings.TrimPrefix(route, "member-")
					body := fmt.Sprintf(`{"message":"manual message","clientUserMessageId":%q}`, planDeliveryID)
					if operation == "queue/reconcile" {
						body = `{}`
					}
					r := httptest.NewRequest(http.MethodPost, "/codex/conversations/"+id+"/"+operation, strings.NewReader(body)).WithContext(ctx)
					r.Header.Set("Origin", f.s.config.BaseURL)
					r.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					f.s.Handler().ServeHTTP(w, r)
					result <- w
				}()
				<-started
				select {
				case response := <-result:
					t.Fatal("request did not wait for exclusive owner", response.Code, response.Body.String())
				case <-time.After(30 * time.Millisecond):
				}
				f.wire.mu.Lock()
				starts, updates := f.wire.starts, f.wire.updates
				f.wire.mu.Unlock()
				if starts != 0 || updates != 0 {
					t.Fatal("waiting request mutated native state", starts, updates)
				}
				switch outcome {
				case "deadline":
					response := <-result
					if response.Code < 400 {
						t.Fatal("deadline proceeded", response.Code)
					}
				case "generation":
					if err := os.Remove(f.s.config.HostProfile); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(t.TempDir(), f.s.config.HostProfile); err != nil {
						t.Fatal(err)
					}
				case "authority":
					if err := os.Remove(filepath.Join(f.s.config.AuthorityDir, "example.json")); err != nil {
						t.Fatal(err)
					}
				case "lifecycle":
					writeWebTrackingFiles(t, f.wire.cwd, "complete")
				case "cold":
					cold.Store(true)
				}
				exclusive.Close()
				if outcome != "deadline" {
					response := <-result
					if outcome == "released" && response.Code >= 400 {
						t.Fatal(response.Code, response.Body.String())
					}
					if outcome != "released" && response.Code < 400 {
						t.Fatal("changed admission proceeded after wait", outcome, response.Code)
					}
				}
				f.wire.mu.Lock()
				starts, updates = f.wire.starts, f.wire.updates
				f.wire.mu.Unlock()
				want := 0
				if outcome == "released" && (route == "plan" || strings.HasSuffix(route, "message")) {
					want = 1
				}
				if starts != want {
					t.Fatal("runtime wait lost delivery exclusion", starts, want)
				}
				if outcome != "released" && updates != 0 {
					t.Fatal("refused wait changed settings")
				}
				check, err := session.LockRuntimeExclusive(f.s.config.AuthorityDir, "example")
				if err != nil {
					t.Fatal("request leaked runtime lock", err)
				}
				check.Close()
			})
		}
	}
}
