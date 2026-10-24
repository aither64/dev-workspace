package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

var retryBackendBrowserCases = []string{
	"policy-scope", "policy-stale-and-lost-reply", "scheduled-now", "stop-and-stale-token",
	"unknown-and-confirmed", "terminal-dismissal", "footer-refresh", "optional-unavailable-manual",
}

// Only display reads/events are adapted. All delivery, admission, policy,
// receipt and retry methods are inherited from the real managed client.
type retryBackendBrowserDisplay struct {
	*planDeliveryClient
	mu        sync.Mutex
	extra     bool
	reads     int
	listeners map[chan struct{}]bool
}

func (c *retryBackendBrowserDisplay) ReadThread(ctx context.Context, thread string) (codex.Transcript, error) {
	p, err := c.ReadThreadPage(ctx, thread, "")
	return p.Transcript, err
}

func (c *retryBackendBrowserDisplay) ReadThreadPage(_ context.Context, thread, cursor string) (codex.TranscriptPage, error) {
	if thread != "thread-1" && thread != "member-thread" {
		return codex.TranscriptPage{}, errors.New("unexpected display thread")
	}
	c.mu.Lock()
	c.reads++
	extra := c.extra
	c.mu.Unlock()
	first, last := 31, 50
	if extra {
		last++
	}
	if cursor != "" {
		if cursor != "backend-older" {
			return codex.TranscriptPage{}, errors.New("unexpected display cursor")
		}
		first, last = 11, 30
	}
	entries := []codex.TranscriptEntry{}
	for i := first; i <= last; i++ {
		entries = append(entries, codex.TranscriptEntry{Kind: "agentMessage", ItemID: fmt.Sprintf("backend-%d", i), TurnID: "display-task", TurnStatus: "completed", Text: fmt.Sprintf("Backend fixture conversation message %d.\n\nRetained display text for scroll verification.", i), Timestamp: time.Unix(1792770000+int64(i), 0).UTC().Format(time.RFC3339)})
	}
	p := codex.TranscriptPage{Transcript: codex.Transcript{ThreadID: thread, LatestTurnID: "display-task", Status: "idle", Model: "retained-model", ReasoningEffort: "xhigh", CollaborationMode: "default", Entries: entries}}
	if cursor == "" {
		older := "backend-older"
		p.OlderCursor, p.HasOlder = &older, true
	}
	return p, nil
}

func (c *retryBackendBrowserDisplay) PromptsWithItems(context.Context, string) ([]codex.Prompt, error) {
	return []codex.Prompt{}, nil
}
func (c *retryBackendBrowserDisplay) ListModels(context.Context) ([]codex.Model, error) {
	return []codex.Model{{ID: "retained-model", Model: "retained-model", DisplayName: "Retained fixture model", IsDefault: true, DefaultReasoningEffort: "xhigh", SupportedReasoningEfforts: []codex.ReasoningEffortOption{{ReasoningEffort: "xhigh"}}}}, nil
}
func (c *retryBackendBrowserDisplay) ListCollaborationModes(context.Context) ([]codex.CollaborationMode, error) {
	return []codex.CollaborationMode{{Name: "Default", Mode: "default"}, {Name: "Plan", Mode: "plan"}}, nil
}
func (c *retryBackendBrowserDisplay) ReadAccountRateLimits(context.Context) (codex.AccountRateLimits, error) {
	return codex.AccountRateLimits{}, nil
}

// The ordinary duration display is separate from retry/activity and from the
// native observation used by RetryBackend and retained mutation admission.
func (c *retryBackendBrowserDisplay) ReadActivity(_ context.Context, thread string) (codex.ActivitySnapshot, error) {
	if thread != "thread-1" && thread != "member-thread" {
		return codex.ActivitySnapshot{}, errors.New("unexpected ordinary activity thread")
	}
	now := time.Now().UnixMilli()
	return codex.ActivitySnapshot{ThreadID: thread, CurrentState: "idle", ObservedAtMS: now, StateSinceMS: now, CountsComplete: true, CoverageComplete: true}, nil
}
func (c *retryBackendBrowserDisplay) ListThreadActivity(_ context.Context, rows []workspacecodex.ThreadActivity) ([]workspacecodex.ThreadActivity, error) {
	return rows, nil
}
func (c *retryBackendBrowserDisplay) Subscribe(_ context.Context, _ string) (<-chan struct{}, func(), error) {
	ch := make(chan struct{}, 1)
	c.mu.Lock()
	c.listeners[ch] = true
	c.mu.Unlock()
	return ch, func() { c.mu.Lock(); delete(c.listeners, ch); c.mu.Unlock() }, nil
}
func (c *retryBackendBrowserDisplay) signal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ch := range c.listeners {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

var _ conversation.Client = (*retryBackendBrowserDisplay)(nil)
var _ teamruntime.Client = (*retryBackendBrowserDisplay)(nil)
var _ turnRetryClient = (*retryBackendBrowserDisplay)(nil)

type retryBackendNativeTurn struct {
	latest, status, code string
	items                []any
	expected             *turnretry.Pending
}

// This wire is private to this fixture: the shared retryWire deliberately
// recognizes only the literal "failed", whereas each scenario needs a new
// exact failure identity. Its methods and effects stay strictly bounded.
type retryBackendNativeWire struct {
	mu     sync.Mutex
	cwd    string
	turns  map[string]*retryBackendNativeTurn
	starts int
	expose bool
}

func (w *retryBackendNativeWire) serve(t *testing.T, socket string) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	host := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(rw, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var request struct {
				ID     any            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(data, &request) != nil {
				t.Error("invalid private native request")
				return
			}
			if request.ID == nil {
				continue
			}
			w.mu.Lock()
			thread, _ := request.Params["threadId"].(string)
			turn := w.turns[thread]
			var result any
			failure := ""
			if request.Method == "initialize" {
				result = map[string]any{"userAgent": "codex/fixture"}
			} else if turn == nil {
				failure = "unexpected private native thread"
			} else {
				switch request.Method {
				case "thread/read":
					result = map[string]any{"thread": map[string]any{"id": thread, "cwd": w.cwd, "source": "vscode", "modelProvider": "fixture", "status": map[string]any{"type": "idle"}}}
				case "thread/turns/list":
					result = map[string]any{"data": []any{map[string]any{"id": turn.latest, "status": turn.status, "error": map[string]any{"codexErrorInfo": turn.code, "message": "synthetic"}}}}
				case "thread/queue/list":
					result = map[string]any{"data": []any{}}
				case "thread/items/list":
					items := []any{}
					if w.expose && turn.items != nil {
						items = turn.items
					}
					result = map[string]any{"data": items, "nextCursor": nil}
				case "turn/start":
					p := turn.expected
					input, _ := request.Params["input"].([]any)
					var text, kind any
					if len(input) == 1 {
						if item, ok := input[0].(map[string]any); ok {
							text, kind = item["text"], item["type"]
						}
					}
					if p == nil || turn.status != "failed" || turn.latest != p.Failure || request.Params["clientUserMessageId"] != p.ID || text != turnretry.Continuation || kind != "text" || request.Params["model"] != p.Snapshot.Options.Model || request.Params["effort"] != p.Snapshot.Options.ReasoningEffort || p.Action != "turn-retry:"+p.Failure+":"+p.Snapshot.Digest+":"+p.ID {
						failure = "native continuation changed exact failure, Pending, input or options"
						break
					}
					for key := range request.Params {
						if key != "threadId" && key != "input" && key != "clientUserMessageId" && key != "model" && key != "effort" {
							failure = "unexpected native continuation parameter"
						}
					}
					if failure != "" {
						break
					}
					w.starts++
					turn.latest, turn.status, turn.expected = "accepted-"+p.ID, "completed", nil
					turn.items = []any{map[string]any{"turnId": turn.latest, "item": map[string]any{"id": "input-" + p.ID, "type": "userMessage", "clientId": p.ID, "content": []any{map[string]any{"type": "text", "text": turnretry.Continuation}}}}}
					// Real synthetic acceptance; lose only the usable native reply.
					result = map[string]any{"turn": map[string]any{}}
				default:
					failure = "unexpected private native method: " + request.Method
				}
			}
			w.mu.Unlock()
			reply := map[string]any{"id": request.ID, "result": result}
			if failure != "" {
				t.Error(failure)
				delete(reply, "result")
				reply["error"] = map[string]any{"code": -32000, "message": failure}
			}
			encoded, _ := json.Marshal(reply)
			if conn.Write(context.Background(), websocket.MessageText, encoded) != nil {
				return
			}
		}
	})}
	go func() { _ = host.Serve(listener) }()
	t.Cleanup(func() { _ = host.Close(); _ = listener.Close() })
}

func newRetryBackendNative(t *testing.T) (*Server, *planDeliveryClient, *retryBackendNativeWire) {
	t.Helper()
	s := newTestServer(t)
	cwd := filepath.Join(s.config.Workspace, "work", "example")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "retry-backend-native-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "native.sock")
	s.config.CodexSocket = socket
	manifest := "schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: thread-1\n  socket_path: " + socket + "\n  client_version: 0.152.1\n"
	if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, cwd, "active")
	writeWebRuntimeAuthority(t, s, "example")
	wire := &retryBackendNativeWire{cwd: cwd, turns: map[string]*retryBackendNativeTurn{}}
	for _, thread := range []string{"thread-1", "member-thread"} {
		wire.turns[thread] = &retryBackendNativeTurn{latest: "initial-" + thread, status: "failed", code: "cyberPolicy"}
	}
	wire.serve(t, socket)
	if err := (session.RecoveryStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Set(context.Background(), "example", "thread-1", socket, true, true); err != nil {
		t.Fatal(err)
	}
	c := workspacecodex.NewWithOptions(socket, s.config.Workspace, codex.ClientOptions{SubmissionLedgerPath: filepath.Join(s.config.UserStateRoot, "native.json")})
	t.Cleanup(c.Close)
	c.RecoveryRoot = s.config.UserStateRoot
	c.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return codex.ThreadPolicy{}, nil }
	managed := &planDeliveryClient{resolverRetryClient: &resolverRetryClient{c, cwd}, stateRoot: s.config.UserStateRoot, workspace: s.config.Workspace}
	f := &planDeliveryFixture{s: s, client: managed}
	s.config.Codex = managed
	addPolicyMember(t, f)
	if _, err := s.transactRetryPolicy(context.Background(), managed, "example", nil); err != nil {
		t.Fatal(err)
	}
	return s, managed, wire
}

type retryBackendBrowserFixture struct {
	t       *testing.T
	s       *Server
	client  *retryBackendBrowserDisplay
	wire    *retryBackendNativeWire
	manual  *planDeliveryWire
	scope   turnretry.Scope
	policy  turnretry.PolicyScope
	path    string
	mu      sync.Mutex
	drop    bool
	posts   int
	dropped int
	seeds   int
}

func newRetryBackendBrowserFixture(t *testing.T, member, manual bool) *retryBackendBrowserFixture {
	t.Helper()
	f := &retryBackendBrowserFixture{t: t}
	if manual {
		p := newPlanDeliveryFixture(t)
		addPolicyMember(t, p)
		f.s, f.manual = p.s, p.wire
		f.client = &retryBackendBrowserDisplay{planDeliveryClient: p.client, listeners: map[chan struct{}]bool{}}
		f.s.config.Codex = f.client
		if _, err := f.s.transactRetryPolicy(context.Background(), f.client, "example", nil); err != nil {
			t.Fatal(err)
		}
		f.path = retryPolicyPath(p)
	} else {
		// The private wire keeps native failure identities exact across seeds.
		s, managed, wire := newRetryBackendNative(t)
		f.s, f.wire = s, wire
		f.client = &retryBackendBrowserDisplay{planDeliveryClient: managed, listeners: map[chan struct{}]bool{}}
		f.s.config.Codex = f.client
		f.path = retryPolicyPath(&planDeliveryFixture{s: s})
	}
	thread := "thread-1"
	if member {
		thread = "member-thread"
	}
	var err error
	f.scope, err = f.client.RetryScope(context.Background(), thread)
	if err != nil {
		t.Fatal(err)
	}
	if f.policy.Incarnation == "" {
		identity, _, err := (retryProvider{client: f.client, scope: f.scope}).policy()
		if err != nil {
			t.Fatal(err)
		}
		f.policy = identity
	}
	if f.s.config.RecoverSessions {
		t.Fatal("fixture must not start automatic recovery/retry workers")
	}
	return f
}

func TestRetryBackendBrowserFixtureConstruction(t *testing.T) {
	for _, manual := range []bool{false, true} {
		for _, member := range []bool{false, true} {
			name := map[bool]string{false: "retry", true: "manual"}[manual] + "-" + map[bool]string{false: "root", true: "member"}[member]
			t.Run(name, func(t *testing.T) {
				f := newRetryBackendBrowserFixture(t, member, manual)
				store := session.RecoveryStore{Root: f.s.config.UserStateRoot, Workspace: f.s.config.Workspace}
				recovery, err := store.Load("example")
				if err != nil {
					t.Fatal(err)
				}
				epoch, err := session.SocketIdentity(f.scope.Socket)
				if err != nil {
					t.Fatal(err)
				}
				if recovery.Schema != 1 || recovery.Workspace != f.scope.Workspace || recovery.Slug != f.scope.Slug || recovery.ThreadID != f.scope.Root || recovery.SocketPath != f.scope.Socket || recovery.SocketIdentity != epoch || !recovery.Automatic || len(recovery.ActiveThreads) != 2 {
					t.Fatal("fixture recovery identity is incomplete", recovery)
				}
				for _, thread := range []string{"thread-1", "member-thread"} {
					if !recovery.Active(thread, epoch) || !store.AllowsImplicitResume(f.scope.Socket, thread) {
						t.Fatal("fixture lacks exact retained execution permission", thread)
					}
				}
				thread, id := "thread-1", "example"
				if member {
					thread, id = "member-thread", "example~implementer0"
				}
				if f.scope.Thread != thread || f.scope.Root != "thread-1" || f.policy.Incarnation == "" || f.s.config.RecoverSessions {
					t.Fatal("fixture scope or worker control changed", f.scope, f.policy)
				}
				for _, path := range []string{"/api/sessions/example/retry-policy", retryActivityPath(id) + "/retry/activity"} {
					w := retryRequest(f.s, context.Background(), http.MethodGet, path, "")
					if w.Code != http.StatusOK {
						t.Fatal("fixture real Handler is unavailable", path, w.Code, w.Body.String())
					}
				}
				path := "/api/sessions/example/retry-policy"
				before := retryRequest(f.s, context.Background(), http.MethodGet, path, "")
				var policy struct {
					Revision      string `json:"revision"`
					Capacity      bool   `json:"capacity"`
					Cybersecurity bool   `json:"cybersecurity"`
				}
				if err := json.Unmarshal(before.Body.Bytes(), &policy); err != nil || !policy.Capacity || !policy.Cybersecurity || policy.Revision == "" {
					t.Fatal("fixture initial policy is invalid", policy, err)
				}
				body, _ := json.Marshal(map[string]any{"capacity": false, "cybersecurity": true, "expectedRevision": policy.Revision})
				changed := retryRequest(f.s, context.Background(), http.MethodPost, path, string(body))
				var saved struct {
					Revision      string `json:"revision"`
					Capacity      bool   `json:"capacity"`
					Cybersecurity bool   `json:"cybersecurity"`
				}
				if err := json.Unmarshal(changed.Body.Bytes(), &saved); err != nil || changed.Code != http.StatusOK || saved.Capacity || !saved.Cybersecurity || saved.Revision == "" || saved.Revision == policy.Revision {
					t.Fatal("fixture actual policy change failed", changed.Code, changed.Body.String(), err)
				}
				readback := retryRequest(f.s, context.Background(), http.MethodGet, path, "")
				if readback.Code != http.StatusOK || readback.Body.String() != changed.Body.String() {
					t.Fatal("fixture actual policy readback changed", readback.Code, readback.Body.String())
				}
				starts := 0
				if manual {
					f.manual.mu.Lock()
					starts = f.manual.starts
					f.manual.mu.Unlock()
				} else {
					f.wire.mu.Lock()
					starts = f.wire.starts
					f.wire.mu.Unlock()
				}
				if starts != 0 {
					t.Fatal("fixture construction dispatched a native turn", starts)
				}
			})
		}
	}
}

func (f *retryBackendBrowserFixture) seed(class string) error {
	ctx := context.Background()
	if err := os.Chmod(filepath.Dir(f.path), 0700); err != nil {
		return err
	}
	f.mu.Lock()
	f.seeds++
	failure := fmt.Sprintf("backend-failure-%d", f.seeds)
	f.mu.Unlock()
	code := "serverOverloaded"
	if class == "cybersecurity" {
		code = "cyberPolicy"
	}
	f.wire.mu.Lock()
	f.wire.turns[f.scope.Thread] = &retryBackendNativeTurn{latest: failure, status: "failed", code: code}
	f.wire.expose = false
	f.wire.mu.Unlock()
	p, err := policyStore(f.client).Load(f.policy)
	if err != nil {
		return err
	}
	tx, err := f.client.RetryStore().Lock(ctx, f.scope)
	if err != nil {
		return err
	}
	defer tx.Close()
	tx.State = turnretry.New(f.scope)
	tx.State.Boundary = f.policy.Incarnation
	tx.State.Enroll(turnretry.Observation{})
	if err := tx.State.AddSnapshot(snapshotForHTTPRetry(failure)); err != nil {
		return err
	}
	now := time.Now()
	if err := tx.State.Observe(turnretry.Observation{Turn: failure, Status: "failed", Code: code}, now, 0, turnretry.Decision{Revision: p.Revision, Capacity: p.Capacity, Cybersecurity: p.Cybersecurity}); err != nil {
		return err
	}
	if tx.State.Pending == nil {
		return errors.New("scenario did not schedule a real Pending")
	}
	if class == "capacity" {
		tx.State.DeadlineMS = now.Add(15 * time.Second).UnixMilli()
	}
	if err := tx.Save(); err != nil {
		return err
	}
	return (turnretry.SessionPolicy{Store: policyStore(f.client), Scope: f.policy}).Checkpoint(ctx, &tx.State, nil, now, true)
}

func (f *retryBackendBrowserFixture) control(operation string) error {
	ctx := context.Background()
	switch operation {
	case "schedule":
		return f.seed("capacity")
	case "unknown":
		if err := f.seed("cybersecurity"); err != nil {
			return err
		}
		tx, err := f.client.RetryStore().Lock(ctx, f.scope)
		if err != nil {
			return err
		}
		p, err := policyStore(f.client).Load(f.policy)
		if err == nil {
			err = tx.State.Begin(time.Now(), turnretry.Decision{Revision: p.Revision, Capacity: p.Capacity, Cybersecurity: p.Cybersecurity})
		}
		if err == nil {
			err = tx.Save()
		}
		pending := tx.State.Pending
		tx.Close()
		if err != nil {
			return err
		}
		f.wire.mu.Lock()
		f.wire.turns[f.scope.Thread].expected = pending
		f.wire.mu.Unlock()
		_, err = f.client.Client.ContinueFailedTurn(ctx, f.scope.Thread, turnretry.Continuation, pending.ID, pending.Action, pending.Failure, pending.Snapshot.Options)
		var unknown *codex.UnknownSendOutcomeError
		if !errors.As(err, &unknown) {
			return fmt.Errorf("expected exact accepted-but-unconfirmed fixture: %w", err)
		}
		return nil
	case "confirm-gap":
		f.wire.mu.Lock()
		f.wire.expose = true
		f.wire.mu.Unlock()
		if err := os.Chmod(filepath.Dir(f.path), 0500); err != nil {
			return err
		}
		controller := turnretry.Controller{Store: f.client.RetryStore(), Backend: f.client.RetryBackend(), Policy: turnretry.SessionPolicy{Store: policyStore(f.client), Scope: f.policy}}
		return controller.ReconcileOnly(ctx, f.scope)
	case "checkpoint":
		if err := os.Chmod(filepath.Dir(f.path), 0700); err != nil {
			return err
		}
		state, err := f.client.RetryStore().Load(f.scope)
		if err != nil {
			return err
		}
		return (turnretry.SessionPolicy{Store: policyStore(f.client), Scope: f.policy}).Checkpoint(ctx, &state, nil, time.Now(), false)
	case "idle", "legacy-off":
		tx, err := f.client.RetryStore().Lock(ctx, f.scope)
		if err != nil {
			return err
		}
		defer tx.Close()
		tx.State = turnretry.New(f.scope)
		tx.State.Boundary = f.policy.Incarnation
		tx.State.Enroll(turnretry.Observation{})
		if operation == "legacy-off" {
			tx.State.SetEnabled(false, turnretry.Observation{})
		}
		return tx.Save()
	case "append":
		f.client.mu.Lock()
		f.client.extra = true
		f.client.mu.Unlock()
		f.client.signal()
		return nil
	case "lose-policy-reply":
		f.mu.Lock()
		f.drop = true
		f.mu.Unlock()
		return nil
	case "corrupt-policy":
		return os.WriteFile(f.path, []byte("{broken optional metadata\n"), 0600)
	}
	return errors.New("unknown private scenario control")
}

func (f *retryBackendBrowserFixture) diagnostics() map[string]any {
	f.mu.Lock()
	posts, dropped := f.posts, f.dropped
	f.mu.Unlock()
	f.client.mu.Lock()
	reads := f.client.reads
	f.client.mu.Unlock()
	state, err := f.client.RetryStore().Load(f.scope)
	stateJSON, _ := json.Marshal(state)
	policyJSON, _ := os.ReadFile(f.path)
	data := map[string]any{"posts": posts, "dropped": dropped, "displayReads": reads, "ledgerHash": fmt.Sprintf("%x", sha256.Sum256(stateJSON)), "policyHash": fmt.Sprintf("%x", sha256.Sum256(policyJSON)), "ledgerPresent": err == nil, "chain": state.Chain, "attempt": state.Attempt, "cancelled": state.Cancelled, "uncertain": state.Uncertain(), "charges": len(state.Cyber)}
	if state.Pending != nil {
		data["pending"], data["result"], data["deadline"] = state.Pending.ID, state.Pending.ResultTurn, state.DeadlineMS
	}
	if f.wire != nil {
		f.wire.mu.Lock()
		data["starts"] = f.wire.starts
		f.wire.mu.Unlock()
	} else {
		f.manual.mu.Lock()
		data["starts"], data["resumes"], data["settingsWrites"] = f.manual.starts, f.manual.resumes, f.manual.updates
		f.manual.mu.Unlock()
	}
	return data
}

func serveRetryBackendBrowser(t *testing.T, f *retryBackendBrowserFixture) *httptest.Server {
	t.Helper()
	host := httptest.NewUnstartedServer(nil)
	f.s.config.BaseURL = "https://" + host.Listener.Addr().String()
	f.s.trustedOrigins = []string{f.s.config.BaseURL}
	var err error
	f.s.conversation, err = conversation.NewHandler(conversation.Options{AllowedOrigins: f.s.trustedOrigins, BasePath: "/codex", Shutdown: f.s.stopping, Resolver: conversation.ResolverFunc(f.s.resolveConversation)})
	if err != nil {
		t.Fatal(err)
	}
	f.s.uploadHandler, err = conversation.NewUploadHandler(conversation.UploadOptions{BasePath: "/uploads", AllowedOrigins: f.s.trustedOrigins, Resolve: f.s.resolveUploads})
	if err != nil {
		t.Fatal(err)
	}
	f.s.uploadStore.MinFreeBytes = 0
	handler := f.s.Handler()
	host.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__retry_backend__/diagnostics" && r.Method == http.MethodGet {
			f.s.writeJSON(w, 200, f.diagnostics())
			return
		}
		if r.URL.Path == "/__retry_backend__/control" && r.Method == http.MethodPost {
			if r.Header.Get("Origin") != f.s.config.BaseURL {
				http.Error(w, "fixture origin mismatch", 403)
				return
			}
			var body struct {
				Operation string `json:"operation"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body) != nil {
				http.Error(w, "invalid private scenario control", 400)
				return
			}
			if err := f.control(body.Operation); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			f.s.writeJSON(w, 200, f.diagnostics())
			return
		}
		if r.Method == http.MethodPost {
			f.mu.Lock()
			if r.URL.Path == "/api/sessions/example/retry-policy" {
				f.posts++
			}
			drop := f.drop && r.URL.Path == "/api/sessions/example/retry-policy"
			if drop {
				f.drop = false
			}
			f.mu.Unlock()
			if drop {
				// Execute the unmodified real handler to completion first. Only
				// transport delivery is lost, never the durable policy mutation.
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				if recorder.Code != 200 {
					f.t.Errorf("lost-reply handler did not commit: %d %s", recorder.Code, recorder.Body.String())
				} else {
					f.mu.Lock()
					f.dropped++
					f.mu.Unlock()
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					f.t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
		}
		handler.ServeHTTP(w, r)
	})
	host.StartTLS()
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(f.path), 0700); host.Close() })
	return host
}

// This is independent from the presentation-only intercepted API gallery.
// Selected execution requires the declared Nix Node/Playwright/browser inputs;
// missing dependencies fail rather than skip the selected verification.
func TestRetryBackendBrowser(t *testing.T) {
	if os.Getenv("PORTAL_BROWSER_TEST") != "1" {
		t.Skip("set PORTAL_BROWSER_TEST=1 to run the real retry backend browser fixture")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal(err)
	}
	outputRoot := os.Getenv("PORTAL_RETRY_BACKEND_OUTPUT")
	if outputRoot == "" {
		outputRoot = t.TempDir()
	}
	all := map[string][]string{}
	for _, name := range []string{"desktop-root", "desktop-member", "mobile-root", "mobile-member"} {
		t.Run(name, func(t *testing.T) {
			member := strings.HasSuffix(name, "member")
			retry := newRetryBackendBrowserFixture(t, member, false)
			manual := newRetryBackendBrowserFixture(t, member, true)
			retryHost, manualHost := serveRetryBackendBrowser(t, retry), serveRetryBackendBrowser(t, manual)
			directory := filepath.Join(outputRoot, name)
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "node", "retry_backend_browser_test.cjs", retryHost.URL, manualHost.URL, name)
			command.Env = append(os.Environ(), "PORTAL_RETRY_BACKEND_OUTPUT="+directory)
			output, err := command.CombinedOutput()
			if writeErr := os.WriteFile(filepath.Join(directory, "browser.log"), output, 0600); writeErr != nil {
				t.Fatal(writeErr)
			}
			if err != nil {
				t.Fatalf("real backend browser: %v\n%s", err, output)
			}
			data, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Name  string   `json:"name"`
				Cases []string `json:"cases"`
			}
			if err := json.Unmarshal(data, &manifest); err != nil || manifest.Name != name || strings.Join(manifest.Cases, ",") != strings.Join(retryBackendBrowserCases, ",") {
				t.Fatalf("incomplete browser case manifest: %s (%v)", data, err)
			}
			all[name] = manifest.Cases
			t.Log("retry-backend-manifest " + string(data))
		})
	}
	data, _ := json.Marshal(map[string]any{"required": 32, "subtests": all})
	if err := os.WriteFile(filepath.Join(outputRoot, "manifest.json"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatal("missing backend browser subtests")
	}
}
