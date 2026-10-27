package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

const retainedInputID = "00000000-0000-4000-8000-000000000091"

func (f *planDeliveryFixture) messageRequest(retry bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"message": "manual message", "clientUserMessageId": retainedInputID, "retry": retry})
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/example/message", strings.NewReader(string(body)))
	r.Header.Set("Origin", f.s.config.BaseURL)
	r.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(out, r)
	return out
}

func prepareOpenHeldInput(t *testing.T, f *planDeliveryFixture, lifecycle string) {
	t.Helper()
	writeWebTrackingFiles(t, f.wire.cwd, lifecycle)
	f.s.config.RecoverSessions = true
	ctx := context.Background()
	if err := f.s.recoveryStore().Set(ctx, "example", "thread-1", f.socket, false, false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.recoveryStore().Set(ctx, "example", "thread-1", f.socket, true, false); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedOpenInputHTTPKeepsExactOriginal(t *testing.T) {
	for _, lifecycle := range []string{"complete", "abandoned"} {
		for _, stage := range []string{"fresh", "prepared", "accepted", "unknown"} {
			t.Run(lifecycle+"/"+stage, func(t *testing.T) {
				f := newPlanDeliveryFixture(t)
				ctx := context.Background()
				if stage == "prepared" {
					if err := f.client.PrepareSend("thread-1", "manual message", retainedInputID, "", false); err != nil {
						t.Fatal(err)
					}
				}
				var original codex.SendReceipt
				if stage == "accepted" || stage == "unknown" {
					f.wire.mu.Lock()
					f.wire.lost = stage == "unknown"
					f.wire.mu.Unlock()
					var err error
					original, err = f.client.Client.Send(ctx, "thread-1", "manual message", retainedInputID, "")
					if (err != nil) != (stage == "unknown") {
						t.Fatal(original, err)
					}
				}
				prepareOpenHeldInput(t, f, lifecycle)
				// Staging a mode is local. The first explicit input must apply it before native dispatch.
				if stage == "fresh" || stage == "prepared" {
					mode := "default"
					c := recoveryConversationClient{Client: f.client, server: f.s, slug: "example", root: "thread-1", thread: "thread-1"}
					if _, err := c.UpdateThreadSettings(ctx, "thread-1", codex.ThreadSettingsUpdate{CollaborationMode: &mode}); err != nil {
						t.Fatal(err)
					}
					f.wire.mu.Lock()
					f.wire.requireMode = mode
					f.wire.mu.Unlock()
				}
				out := f.messageRequest(stage != "fresh")
				if stage == "unknown" {
					if out.Code != http.StatusConflict || !strings.Contains(out.Body.String(), `"code":"message_delivery_unknown"`) || strings.Contains(out.Body.String(), `"notSent":true`) {
						t.Fatal("unknown original relabeled or dispatched", out.Code, out.Body.String())
					}
				} else {
					if out.Code != http.StatusAccepted {
						t.Fatal(out.Code, out.Body.String())
					}
					var receipt codex.SendReceipt
					if err := json.Unmarshal(out.Body.Bytes(), &receipt); err != nil || receipt.ClientUserMessageID != retainedInputID {
						t.Fatal(receipt, err)
					}
					if stage == "accepted" && receipt != original {
						t.Fatal("original receipt changed", receipt, original)
					}
					if again := f.messageRequest(true); again.Code != http.StatusAccepted {
						t.Fatal(again.Code, again.Body.String())
					}
				}
				f.wire.mu.Lock()
				starts, queues := f.wire.starts, f.wire.queues
				f.wire.mu.Unlock()
				if starts != 1 || queues != 0 {
					t.Fatal("original input replayed or queued", starts, queues)
				}
				summary, err := session.Find(f.s.config.Workspace, "example")
				if err != nil || summary.Lifecycle != lifecycle || summary.Archived {
					t.Fatal("input changed lifecycle", summary, err)
				}
			})
		}
	}
}

func TestRetainedOpenInputHTTPRejectsChangedAuthorityAndArchive(t *testing.T) {
	for _, change := range []string{"authority", "version", "archive"} {
		t.Run(change, func(t *testing.T) {
			f := newPlanDeliveryFixture(t)
			prepareOpenHeldInput(t, f, "complete")
			if change == "archive" {
				path := filepath.Join(f.wire.cwd, "portal.yml")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(data, []byte("finalized_at: \"2026-10-27T00:00:00Z\"\n")...), 0600); err != nil {
					t.Fatal(err)
				}
				parent := filepath.Join(f.s.config.Workspace, "archive")
				if err := os.MkdirAll(parent, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(f.wire.cwd, filepath.Join(parent, "example")); err != nil {
					t.Fatal(err)
				}
			} else {
				authority, err := session.LoadRuntimeAuthority(f.s.config.AuthorityDir, "example", f.s.config.Workspace)
				if err != nil {
					t.Fatal(err)
				}
				if change == "authority" {
					authority.CodexThreadID = "different-root"
				} else {
					authority.CodexClientVersion = "0.160.0"
				}
				data, _ := json.Marshal(authority)
				if err := os.WriteFile(filepath.Join(f.s.config.AuthorityDir, "example.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			out := f.messageRequest(true)
			if out.Code < 400 {
				t.Fatal(out.Code, out.Body.String())
			}
			f.wire.mu.Lock()
			defer f.wire.mu.Unlock()
			if f.wire.starts != 0 || f.wire.queues != 0 || f.wire.resumes != 0 {
				t.Fatal("invalid identity dispatched", f.wire.starts, f.wire.queues, f.wire.resumes)
			}
		})
	}
}

func TestRetainedOpenPlanPolicyRemainsActiveOnly(t *testing.T) {
	for _, lifecycle := range []string{"complete", "abandoned"} {
		t.Run(lifecycle, func(t *testing.T) {
			f := newPlanDeliveryFixture(t)
			prepareOpenHeldInput(t, f, lifecycle)
			out := f.request(context.Background(), planDeliveryID, "plan-turn", "same")
			if out.Code < 400 {
				t.Fatal(fmt.Sprint("plan admitted from ", lifecycle), out.Code)
			}
			f.wire.mu.Lock()
			defer f.wire.mu.Unlock()
			if f.wire.starts != 0 || f.wire.updates != 0 || f.wire.resumes != 0 {
				t.Fatal("plan policy changed", f.wire.starts, f.wire.updates, f.wire.resumes)
			}
		})
	}
}

// Uses the same production HTTP handler and pinned client as the Go route
// regressions above. Each browser/scenario has independent disposable state.
func TestRetainedOpenInputBrowser(t *testing.T) {
	if os.Getenv("PORTAL_BROWSER_TEST") != "1" {
		t.Skip("set PORTAL_BROWSER_TEST=1 with Nix Playwright")
	}
	for _, engine := range []string{"chromium", "firefox"} {
		for _, scenario := range []string{"accepted", "unknown"} {
			t.Run(engine+"/"+scenario, func(t *testing.T) {
				f := newPlanDeliveryFixture(t)
				ctx := context.Background()
				f.wire.mu.Lock()
				f.wire.expectedInput = "say hi"
				f.wire.latest = "prior-turn"
				f.wire.mode = "default" // The browser selects Plan; require a real native mode change before Send.
				f.wire.writeMode()
				f.wire.hide = true // Keep browser receipt confirmation separate from later transcript observation.
				f.wire.mu.Unlock()
				if scenario == "unknown" {
					f.wire.mu.Lock()
					f.wire.lost = true
					f.wire.mu.Unlock()
					_, err := f.client.Client.Send(ctx, "thread-1", "say hi", retainedInputID, "")
					var unknown *codex.UnknownSendOutcomeError
					if !errors.As(err, &unknown) {
						t.Fatal("fixture must retain a real uncertain attempt", err)
					}
					f.wire.mu.Lock()
					f.wire.status = "completed"
					f.wire.mu.Unlock() // Execution settled, but its submission receipt is still unproven.
				}
				prepareOpenHeldInput(t, f, "complete")
				f.wire.mu.Lock()
				f.wire.checkHeld = true
				f.wire.holdUntilReceipt = true
				f.wire.calls = nil
				if scenario == "accepted" {
					f.wire.requireMode = "plan"
				}
				f.wire.mu.Unlock()
				f.s.uploadStore.MinFreeBytes = 0
				httpServer := httptest.NewUnstartedServer(nil)
				f.s.config.BaseURL = "https://" + httpServer.Listener.Addr().String()
				f.s.trustedOrigins = []string{f.s.config.BaseURL}
				handler, err := conversation.NewHandler(conversation.Options{AllowedOrigins: f.s.trustedOrigins, BasePath: "/codex", Shutdown: f.s.stopping, Resolver: conversation.ResolverFunc(f.s.resolveConversation)})
				if err != nil {
					t.Fatal(err)
				}
				f.s.conversation = handler
				f.s.uploadHandler, err = conversation.NewUploadHandler(conversation.UploadOptions{BasePath: "/uploads", AllowedOrigins: f.s.trustedOrigins, Resolve: f.s.resolveUploads})
				if err != nil {
					t.Fatal(err)
				}
				root := f.s.Handler()
				var requestMu sync.Mutex
				var requests []struct {
					Message     string   `json:"message"`
					ID          string   `json:"clientUserMessageId"`
					Retry       bool     `json:"retry"`
					Attachments []string `json:"attachmentIds"`
				}
				httpServer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && r.URL.Path == "/api/sessions/example/message" {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						r.Body = io.NopCloser(bytes.NewReader(body))
						var request struct {
							Message     string   `json:"message"`
							ID          string   `json:"clientUserMessageId"`
							Retry       bool     `json:"retry"`
							Attachments []string `json:"attachmentIds"`
						}
						if err := json.Unmarshal(body, &request); err != nil {
							t.Error(err)
						}
						requestMu.Lock()
						requests = append(requests, request)
						requestMu.Unlock()
					}
					root.ServeHTTP(w, r)
				})
				httpServer.StartTLS()
				defer httpServer.Close()
				command := exec.Command("node", "recovery_input_browser_test.cjs", httpServer.URL, scenario, engine)
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("%s: %v\n%s", scenario, err, output)
				}
				t.Log(string(output))
				requestMu.Lock()
				defer requestMu.Unlock()
				if len(requests) != 1 || requests[0].ID != retainedInputID || requests[0].Message != "say hi" {
					t.Fatal("browser changed original or replayed", requests)
				}
				f.wire.mu.Lock()
				defer f.wire.mu.Unlock()
				if f.wire.starts != 1 || f.wire.queues != 0 || f.wire.messageID != retainedInputID {
					t.Fatal("native replay or identity changed", f.wire.starts, f.wire.queues, f.wire.messageID)
				}
				if scenario == "accepted" {
					if requests[0].Retry || len(requests[0].Attachments) != 1 || !f.wire.holdUntilReceipt || f.s.recoveryHeld("example", "thread-1") {
						t.Fatal("cold delivery ordering or attachments changed", requests, f.wire.calls)
					}
					mode, send := slices.Index(f.wire.calls, "mode"), slices.Index(f.wire.calls, "send")
					if mode < 0 || send <= mode {
						t.Fatal("mode did not precede exact send", f.wire.calls)
					}
					if !strings.Contains(f.wire.message, requests[0].Attachments[0]) {
						t.Fatal("original attachment is missing from native input", requests[0].Attachments, f.wire.message)
					}
				} else if !requests[0].Retry || len(f.wire.calls) != 0 || !f.s.recoveryHeld("example", "thread-1") {
					t.Fatal("unknown original activated", requests, f.wire.calls)
				}
			})
		}
	}
}
