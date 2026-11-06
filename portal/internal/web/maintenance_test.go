package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

const maintenanceWebID = "11111111-1111-4111-8111-111111111111"

func maintenanceWebFixture(t *testing.T) (*Server, maintenance.Store) {
	t.Helper()
	s := newTestServer(t)
	return s, createWebMaintenance(t, s)
}

func createWebMaintenance(t *testing.T, s *Server) maintenance.Store {
	t.Helper()
	store := maintenance.Store{Root: s.config.UserStateRoot, Profile: s.config.HostProfile}
	op := maintenance.Operation{Schema: maintenance.Schema, OperationID: maintenanceWebID, Revision: 1, Phase: "pausing", Profile: store.Profile,
		SourceProfileToken: "PRIVATE-TOKEN", SourcePackage: "/PRIVATE-SOURCE", CandidatePackage: "/PRIVATE-CANDIDATE", PlansDigest: "private-plans",
		Services:     []maintenance.Service{{Workspace: s.config.Workspace, Unit: "private-native-unit", Native: "/private-native", PID: 123, ProcessIdentity: "private-pid", Socket: "/private-socket", SocketEpoch: "private-epoch"}},
		Participants: []maintenance.Participant{{Workspace: s.config.Workspace, Slug: "private-session", Incarnation: "private-incarnation", Root: "private-root-thread", Thread: "private-member-thread", Cwd: filepath.Join(s.config.Workspace, "work/private-session"), SourceTurn: "private-turn", SocketEpoch: "private-epoch", NoticeID: maintenanceWebID, NoticeText: "PRIVATE-NOTICE", NoticeAction: "private-action", NoticeDisposition: "accepted", ContinuationID: maintenanceWebID, ContinuationDisposition: "pending", Reason: "wait-revoked"}}, Blockers: []string{"PRIVATE-BLOCKER"}}
	if err := store.Create(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	return store
}

func maintenanceWebRequest(s *Server, method, path, body, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestMaintenanceAllowsExistingPromptResponse(t *testing.T) {
	for _, path := range []string{"/codex/conversations/example/respond", "/api/sessions/example/respond"} {
		t.Run(path, func(t *testing.T) {
			s := newTestServer(t)
			cwd := filepath.Join(s.config.Workspace, "work", "example")
			// Use the configured SDK and its real offer-token responder. Only the
			// App Server peer is synthetic; HTTP routing and resolution stay real.
			socketDir, err := os.MkdirTemp("", "web-maintenance-prompt-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
			socket := filepath.Join(socketDir, "native.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			responses := make(chan map[string]any, 4)
			connections := make(chan *websocket.Conn, 2)
			peer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				connections <- conn
				write := func(value any) error {
					data, err := json.Marshal(value)
					if err != nil {
						return err
					}
					return conn.Write(r.Context(), websocket.MessageText, data)
				}
				for {
					_, data, err := conn.Read(r.Context())
					if err != nil {
						return
					}
					var req map[string]any
					if err := json.Unmarshal(data, &req); err != nil {
						t.Error(err)
						return
					}
					if req["method"] == nil {
						responses <- req
						continue
					}
					var result any = map[string]any{}
					switch req["method"] {
					case "initialize":
					case "initialized":
						for _, thread := range []string{"thread-1", "foreign-thread"} {
							if err := write(map[string]any{"id": thread + "-question", "method": "item/tool/requestUserInput", "params": map[string]any{
								"threadId": thread, "turnId": "existing-turn", "itemId": "question-item", "isBlocking": true,
								"questions": []any{map[string]any{"id": "choice", "header": "Choice", "question": "Choose", "options": []any{map[string]any{"label": "First", "description": "First option"}}}},
							}}); err != nil {
								return
							}
						}
						continue
					case "thread/read":
						params, _ := req["params"].(map[string]any)
						result = map[string]any{"thread": map[string]any{"id": params["threadId"], "cwd": cwd, "source": "vscode"}}
					default:
						t.Errorf("unexpected native call during prompt completion: %v", req["method"])
					}
					if err := write(map[string]any{"id": req["id"], "result": result}); err != nil {
						return
					}
				}
			})}
			go func() { _ = peer.Serve(listener) }()
			t.Cleanup(func() { _ = peer.Close() })
			client := workspacecodex.NewWithOptions(socket, s.config.Workspace, codex.ClientOptions{SubmissionLedgerPath: filepath.Join(t.TempDir(), "submissions.json")})
			client.CodexHome = t.TempDir()
			t.Cleanup(client.Close)
			s.config.CodexSocket = socket
			prepareInteractiveConversation(t, s, "example")
			manifestPath := filepath.Join(cwd, "portal.yml")
			manifest, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			manifest = []byte(strings.ReplaceAll(string(manifest), "/run/dev-workspace-codex/app-server.sock", socket))
			if err := os.WriteFile(manifestPath, manifest, 0644); err != nil {
				t.Fatal(err)
			}
			s.config.Codex, s.config.VerifyThread = client, client.VerifyThread
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := client.Ensure(ctx); err != nil {
				t.Fatal(err)
			}
			awaitPrompt := func(thread, previousToken string) codex.Prompt {
				t.Helper()
				for {
					prompts := client.Prompts(thread)
					if len(prompts) == 1 && prompts[0].Token != previousToken {
						return prompts[0]
					}
					select {
					case <-ctx.Done():
						t.Fatal("pending prompt was not observed")
					case <-time.After(time.Millisecond):
					}
				}
			}
			prompt := awaitPrompt("thread-1", "")
			foreign := awaitPrompt("foreign-thread", "")
			// Both offers must exist before maintenance starts holding admissions.
			createWebMaintenance(t, s)
			answer := func(p codex.Prompt, token, origin string) *httptest.ResponseRecorder {
				t.Helper()
				body, err := json.Marshal(codex.PromptResponse{ID: p.ID, Token: token, Answers: map[string]map[string][]string{"choice": {"answers": {"First"}}}})
				if err != nil {
					t.Fatal(err)
				}
				return maintenanceWebRequest(s, http.MethodPost, path, string(body), origin)
			}
			for _, check := range []struct {
				name, token, origin string
				status              int
			}{
				{"missing-token", "", s.config.BaseURL, 409},
				{"stale-token", "obsolete", s.config.BaseURL, 409},
				{"foreign-origin", prompt.Token, "https://foreign.test", 403},
				{"missing-origin", prompt.Token, "", 403},
			} {
				t.Run(check.name, func(t *testing.T) {
					w := answer(prompt, check.token, check.origin)
					if w.Code != check.status {
						t.Fatalf("%d want %d: %s", w.Code, check.status, w.Body.String())
					}
				})
			}
			if w := answer(foreign, foreign.Token, s.config.BaseURL); w.Code != 503 || !strings.Contains(w.Body.String(), "Codex service is unavailable") {
				t.Fatalf("foreign prompt did not reach thread-bound refusal: %d %s", w.Code, w.Body.String())
			}
			changed := strings.Replace(string(manifest), "thread_id: thread-1", "thread_id: changed-thread", 1)
			if err := os.WriteFile(manifestPath, []byte(changed), 0644); err != nil {
				t.Fatal(err)
			}
			if w := answer(prompt, prompt.Token, s.config.BaseURL); w.Code != 404 {
				t.Fatalf("changed current thread admitted: %d %s", w.Code, w.Body.String())
			}
			if err := os.WriteFile(manifestPath, manifest, 0644); err != nil {
				t.Fatal(err)
			}
			for _, held := range []struct{ method, path string }{
				{"POST", "/codex/conversations/example/message"}, {"POST", "/codex/conversations/example/queue"},
				{"POST", "/codex/conversations/example/queue/start"}, {"POST", "/codex/conversations/example/settings"},
				{"POST", "/codex/conversations/example/retry/now"}, {"POST", "/api/sessions/example/message"},
				{"POST", "/api/codex-limits/reset"}, {"POST", "/unrelated/respond"},
				{"POST", "/codex/conversations/example/extra/respond"}, {"DELETE", path},
			} {
				if w := maintenanceWebRequest(s, held.method, held.path, `{}`, s.config.BaseURL); w.Code != 503 {
					t.Fatalf("new/unrelated mutation escaped hold: %s %s: %d %s", held.method, held.path, w.Code, w.Body.String())
				}
			}
			select {
			case response := <-responses:
				t.Fatalf("rejected answer reached native peer: %+v", response)
			default:
			}
			if w := answer(prompt, prompt.Token, s.config.BaseURL); w.Code != 200 {
				t.Fatalf("preexisting prompt answer held: %d %s", w.Code, w.Body.String())
			}
			select {
			case response := <-responses:
				if response["id"] != "thread-1-question" || !reflect.DeepEqual(response["result"], map[string]any{"answers": map[string]any{"choice": map[string]any{"answers": []any{"First"}}}}) {
					t.Fatalf("native response: %+v", response)
				}
			case <-ctx.Done():
				t.Fatal("valid answer never reached native peer")
			}
			if len(client.Prompts("thread-1")) != 0 || len(client.Prompts("foreign-thread")) != 1 {
				t.Fatal("answer did not retire only its own pending prompt")
			}
			// Reconnect and reissue the same protocol ID: the old generation's
			// offer must not authorize answering its replacement during the hold.
			_ = (<-connections).CloseNow()
			for len(client.Prompts("foreign-thread")) != 0 {
				select {
				case <-ctx.Done():
					t.Fatal("old prompt generation was not retired")
				case <-time.After(time.Millisecond):
				}
			}
			if err := client.Ensure(ctx); err != nil {
				t.Fatal(err)
			}
			current := awaitPrompt("thread-1", prompt.Token)
			if w := answer(prompt, prompt.Token, s.config.BaseURL); w.Code != 409 {
				t.Fatalf("old generation token admitted: %d %s", w.Code, w.Body.String())
			}
			if w := answer(current, current.Token, s.config.BaseURL); w.Code != 200 {
				t.Fatalf("current generation answer held: %d %s", w.Code, w.Body.String())
			}
			select {
			case response := <-responses:
				if response["id"] != "thread-1-question" || response["result"] == nil {
					t.Fatalf("replacement response: %+v", response)
				}
			case <-ctx.Done():
				t.Fatal("replacement answer never reached native peer")
			}
		})
	}
}

type maintenanceMonitorTransport struct {
	monitorWorkerTransport
	native     monitoroperation.NativeDelivery
	reconciled int
}

func (f *maintenanceMonitorTransport) Ready(_ context.Context, owner monitoroperation.Owner) (string, error) {
	if err := f.native.Admission(owner); err != nil {
		return "maintenance hold", nil
	}
	return "", nil
}
func (f *maintenanceMonitorTransport) Reconcile(_ context.Context, _ monitoroperation.Owner, event monitoroperation.Event) (codex.SendReceipt, bool, error) {
	f.reconciled++
	return codex.SendReceipt{ClientUserMessageID: event.Delivery.MessageID, TurnID: "original-accepted-turn"}, true, nil
}

func TestMaintenanceMonitorDeliveryHoldsNewEffectsAndReconcilesOriginalReceipt(t *testing.T) {
	s, _ := maintenanceWebFixture(t)
	defer s.Close()
	s.config.Codex = monitorPortalClient{}
	production, err := s.monitorDeliveryService()
	if err != nil {
		t.Fatal(err)
	}
	native := production.Transport.(monitoroperation.NativeDelivery)
	owner := monitoroperation.Owner{Slug: "utility-owner", RootThreadID: "lead", Incarnation: "incarnation", OriginTurnID: "origin", Socket: "/socket", SocketEpoch: "epoch", RuntimeSchema: 1}
	client := &monitorWorkerLaunchClient{projects: map[string]string{}, threads: map[string]codex.ThreadMetadata{}}
	launch := &monitoroperation.Service{Store: production.Store, Client: client,
		ResolveLead: func(context.Context, string) (monitoroperation.Owner, error) { return owner, nil }, Resolve: func(string) (monitoroperation.Owner, error) { return owner, nil }, LockSession: func(context.Context, string) (func(), error) { return func() {}, nil },
		Admit: func(context.Context, monitoroperation.Owner) error { return nil }, Activate: func(context.Context, monitoroperation.Owner, string) error { return nil },
		Policy: func(context.Context, monitoroperation.Owner) (monitoroperation.UtilityPolicy, error) {
			return monitoroperation.UtilityPolicy{PackageRoot: s.config.Workspace, CatalogDigest: strings.Repeat("a", 64), NativeName: "watcher", Model: "gpt-6-luna", Effort: "low", Instructions: "Observe only", Sandbox: "danger-full-access", ApprovalPolicy: "never", MaxConcurrent: 1}, nil
		}}
	ctx := context.Background()
	op, err := launch.Start(ctx, owner.RootThreadID, monitoroperation.StartRequest{ID: strings.Repeat("1", 32), Brief: "Exact disposable operation", Source: monitoroperation.Source{Kind: "local", Argv: []string{"fixture"}, Cwd: s.config.Workspace, InputIdentity: "fixture", Revision: "fixture", InvocationID: strings.Repeat("1", 32)}})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	op, err = launch.Report(ctx, op.Launch.ThreadID, monitoroperation.ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &zero})
	if err != nil {
		t.Fatal(err)
	}
	transport := &maintenanceMonitorTransport{native: native}
	delivery := &monitoroperation.DeliveryService{Store: production.Store, Resolve: launch.Resolve, LockSession: launch.LockSession, Transport: transport}
	if err = delivery.Dispatch(ctx, op.ID, op.Events[0].ID); err != nil {
		t.Fatal(err)
	}
	saved, err := production.Store.Get(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CanceledAt != nil || saved.Events[0].Delivery.Status != "held" || transport.sends != 0 {
		t.Fatal("maintenance canceled or dispatched outcome", saved)
	}
	// The same original submission may be discovered during the hold. Its
	// receipt is reconciled before Ready and cannot be replaced by a new send.
	tx, err := production.Store.Lock(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Operation.Events[0].Delivery.Status = "submitting"
	options := codex.TurnOptions{Model: "retained-model", ReasoningEffort: "xhigh"}
	tx.Operation.Events[0].Delivery.Options = &options
	if err = tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	if err = delivery.Dispatch(ctx, op.ID, op.Events[0].ID); err != nil {
		t.Fatal(err)
	}
	saved, err = production.Store.Get(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Events[0].Delivery.Status != "accepted" || saved.Events[0].Delivery.MessageID != op.Events[0].Delivery.MessageID || !reflect.DeepEqual(*saved.Events[0].Delivery.Options, options) || transport.reconciled != 1 || transport.sends != 0 {
		t.Fatal("original receipt did not settle during hold", saved)
	}
}
