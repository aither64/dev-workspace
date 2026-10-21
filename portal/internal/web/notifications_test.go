package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func testPushSubscription(t *testing.T) webpush.Subscription {
	t.Helper()
	_, public, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	return webpush.Subscription{Endpoint: "https://updates.push.services.mozilla.com/wpush/v2/private-capability", Keys: webpush.Keys{P256dh: public, Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}}
}

func testNotifications(t *testing.T) *notificationService {
	t.Helper()
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", filepath.Join(t.TempDir(), "no-provider-catalog"))
	server := newTestServer(t)
	t.Cleanup(server.Close)
	store, err := newNotificationStore(server.operationStore)
	if err != nil {
		t.Fatal(err)
	}
	n := &notificationService{server: server, store: store, client: guardedPushClient()}
	server.notifications = n
	return n
}

func notificationRequest(n *notificationService, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://workspace.example"+path, strings.NewReader(body))
	r.Header.Set("Origin", "https://workspace.example")
	n.server.trustedOrigins = []string{"https://workspace.example"}
	w := httptest.NewRecorder()
	n.server.Handler().ServeHTTP(w, r)
	return w
}

func TestNotificationsPageIsSeparateFromIndex(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.config.DevSession = filepath.Join(t.TempDir(), "unavailable-dev-session")
	index := httptest.NewRecorder()
	server.Handler().ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	if index.Code != http.StatusOK {
		t.Fatalf("index: %d: %s", index.Code, index.Body.String())
	}
	_, menu, ok := strings.Cut(index.Body.String(), `<nav id="sidebar-menu"`)
	menu, _, _ = strings.Cut(menu, "</nav>")
	if !ok || !strings.Contains(menu, `href="/browser-notifications"`) {
		t.Fatal("browser notifications must be reachable through the workspace menu")
	}
	for _, removed := range []string{`id="browser-notifications"`, "data-notification-test"} {
		if strings.Contains(index.Body.String(), removed) {
			t.Fatalf("index contains full-panel controls: %s", removed)
		}
	}
	if !strings.Contains(index.Body.String(), `name="browserNotificationsEnabled" type="checkbox" checked`) {
		t.Fatal("new-session notification preference must remain available")
	}
	for _, required := range []string{"data-notification-delivery", "data-notification-sound", `aria-describedby="menu-notification-error"`, `data-notification-menu-error class="muted" role="status" hidden`, "> Notifications</label>", "> Sound</label>", `href="/browser-notifications">Notification settings</a>`} {
		if !strings.Contains(menu, required) {
			t.Fatalf("compact menu lacks %s", required)
		}
	}
	if strings.Count(menu, `type="checkbox"`) != 2 {
		t.Fatal("compact notification menu must have exactly two checkbox rows")
	}
	for _, removed := range []string{"data-notification-enable", "data-notification-disable", "data-notification-status", "data-notification-worker-status", "data-notification-sound-status", "<button", "Checking", "Page title", "Turn off", "Play sound"} {
		if strings.Contains(menu, removed) {
			t.Fatalf("compact menu contains detailed controls or help: %s", removed)
		}
	}
	if strings.Count(index.Body.String(), `src="/static/browser-notifications.js?v=6" defer`) != 1 {
		t.Fatal("index must initialize one notification owner")
	}
	page := httptest.NewRecorder()
	server.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/browser-notifications", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("notification page: %d: %s", page.Code, page.Body.String())
	}
	for _, required := range []string{`id="browser-notifications"`, "data-notification-enable", "data-notification-test", "data-notification-disable", `type="checkbox" data-notification-sound aria-describedby="notification-sound-hint notification-sound-status" disabled`, `data-notification-sound-status role="status"`, `src="/static/browser-notifications.js?v=6" defer`, `href="/"`} {
		if !strings.Contains(page.Body.String(), required) {
			t.Fatalf("notification page lacks %s", required)
		}
	}
	if strings.Index(page.Body.String(), `src="/static/session-attention-store.js?v=4"`) < 0 ||
		strings.Index(page.Body.String(), `src="/static/session-attention-store.js?v=4"`) > strings.Index(page.Body.String(), `src="/static/browser-notifications.js?v=6"`) {
		t.Fatal("notification page must initialize the shared store before the client")
	}
}

func TestNotificationsPersistenceFailureAndKeyRetention(t *testing.T) {
	n := testNotifications(t)
	store := n.store
	private, public := store.state.PrivateKey, store.state.PublicKey
	store.mu.Lock()
	err := store.change(func(state *notificationState) error {
		state.Preferences["example"] = notificationPreference{Identity: strings.Repeat("a", 64), Enabled: false}
		return nil
	})
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := newNotificationStore(n.server.operationStore)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.state.PrivateKey != private || reopened.state.PublicKey != public || notificationEnabled(&reopened.state, "example", strings.Repeat("a", 64)) {
		t.Fatal("restart lost private key or mute")
	}
	info, _ := os.Stat(store.path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("state not private")
	}
	store.path = filepath.Join(t.TempDir(), "missing", "state.json")
	store.mu.Lock()
	err = store.change(func(state *notificationState) error { delete(state.Preferences, "example"); return nil })
	store.mu.Unlock()
	if err == nil || !store.failed || notificationEnabled(&store.state, "example", strings.Repeat("a", 64)) {
		t.Fatal("failed write erased mute or did not fence delivery")
	}
}

func TestNotificationsCorruptMuteAndRestartWarmupFailClosed(t *testing.T) {
	n := testNotifications(t)
	n.store.state.Preferences["example"] = notificationPreference{Identity: strings.Repeat("a", 64), Enabled: false}
	if err := n.store.save(n.store.state); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(n.store.path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"enabled":false`, `"enabled":null`, 1))
	if err := os.WriteFile(n.store.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newNotificationStore(n.server.operationStore); err == nil {
		t.Fatal("invalid persisted mute treated as default enabled")
	}
	now := time.Now()
	o := attentionFixture()
	o.Lead.LatestTurnStatus = "inProgress"
	o.Lead.CurrentState = "working"
	b, _ := reduceAttention(attentionBaseline{}, o, now)
	o.Lead.LatestTurnStatus = "completed"
	o.Lead.CurrentState = "idle"
	b, _ = reduceAttention(b, o, now.Add(time.Second))
	id := b.Event.ID
	o.Fresh = false
	b, emit := reduceAttention(b, o, now.Add(2*time.Second))
	if emit || b.Fresh || b.Event == nil || b.Event.ID != id {
		t.Fatal("observer warmup discarded committed event")
	}
	o.Fresh = true
	b, emit = reduceAttention(b, o, now.Add(3*time.Second))
	if emit || b.Event.ID != id {
		t.Fatal("warmup replayed or lost committed completion")
	}
}

func TestNotificationsPushExpiryDoesNotClearTitle(t *testing.T) {
	n := testNotifications(t)
	prepareInteractiveConversation(t, n.server, "example")
	summary, err := session.Find(n.server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := session.ObservationIdentity(n.server.config.Workspace, "example", filepath.Join(n.server.config.Workspace, "work", "example"), summary.Codex.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	browserID := strings.Repeat("b", 64)
	event := attentionEvent{Version: 1, ID: attentionDigest(identity, "done", "turn"), Identity: identity, Revision: 1, Slug: "example", Kind: "done", Route: "/example/#codex", ExpiresAt: now.Add(-time.Minute).UnixMilli()}
	n.store.state.Sessions["example"] = attentionBaseline{Identity: identity, RootThread: "thread-1", LeadTurn: "turn", CompletedTurn: "turn", ArmedTurn: "turn", Revision: 1, ObservedAt: now.Add(-6 * time.Minute).UnixMilli(), Eligible: true, Fresh: true, Event: &event, Requests: []string{}}
	n.store.state.Browsers[browserID] = browserRegistration{ID: browserID, Origin: "https://workspace.example", Subscription: testPushSubscription(t)}
	n.store.state.Outbox[notificationDeliveryKey(browserID, "example")] = notificationDelivery{Browser: browserID, Event: event}
	n.server.activity = &activityMonitor{server: n.server, observer: &attentionObserverFixture{testActivityObserver: &testActivityObserver{}}, snapshots: map[string]activityCachedSnapshot{
		"thread-1": {at: now, value: codex.ActivitySnapshot{ThreadID: "thread-1", CurrentTurnID: "turn", LatestTurnStatus: "completed", CurrentState: "idle", ObservedAtMS: now.UnixMilli()}}}}
	if !n.reconcile(now) || len(n.store.state.Outbox) != 0 {
		t.Fatal("expired desktop delivery retained")
	}
	if actual := n.store.state.Sessions["example"].Event; actual == nil || actual.ID != event.ID || actual.Revision != event.Revision {
		t.Fatal("desktop expiry cleared unread title")
	}
}

func TestNotificationsFullOutboxCoalescesBeforeCapacityAdmission(t *testing.T) {
	now := time.Now()
	browserID := strings.Repeat("b", 64)
	state := notificationState{Browsers: map[string]browserRegistration{browserID: {ID: browserID}}, Preferences: map[string]notificationPreference{},
		Sessions: make(map[string]attentionBaseline), Outbox: make(map[string]notificationDelivery)}
	observations := make(map[string]attentionObservation)
	for i := 0; i < 512; i++ {
		slug := fmt.Sprintf("session-%d", i)
		o := attentionFixture()
		o.Slug = slug
		o.Identity = attentionDigest(slug)
		event := attentionEvent{Version: 1, ID: attentionDigest(o.Identity, "done", o.Lead.CurrentTurnID), Identity: o.Identity, Slug: slug, Revision: 1, Kind: "done", ExpiresAt: now.Add(time.Minute).UnixMilli()}
		state.Sessions[slug] = attentionBaseline{Identity: o.Identity, RootThread: o.RootThread, LeadTurn: o.Lead.CurrentTurnID, CompletedTurn: o.Lead.CurrentTurnID,
			ObservedAt: now.Add(-time.Second).UnixMilli(), Eligible: true, Fresh: true, Revision: 1, Requests: []string{}, Event: &event}
		state.Outbox[notificationDeliveryKey(browserID, slug)] = notificationDelivery{Browser: browserID, Event: event}
		observations[slug] = o
	}
	input := observations["session-0"]
	input.Lead.CurrentTurnID = "new-turn"
	input.Lead.LatestTurnStatus = "inProgress"
	input.Lead.CurrentState = "waiting"
	input.Requests = []attentionRequest{{"new-blocking-request", "lead", "input"}}
	observations[input.Slug] = input
	reconcileNotificationState(&state, observations, false, now)
	key := notificationDeliveryKey(browserID, input.Slug)
	if len(state.Outbox) != 512 || state.Outbox[key].Event.Kind != "input" || state.Outbox[key].Event.ID != state.Sessions[input.Slug].Event.ID {
		t.Fatal("full outbox consumed higher-priority input without replacing completion")
	}
	// A different new key must get the space reclaimed from expired work in the
	// same transaction, rather than consume its emission before cleanup.
	oldKey := notificationDeliveryKey(browserID, "session-1")
	expired := state.Outbox[oldKey]
	expired.Event.ExpiresAt = now.Add(-time.Second).UnixMilli()
	state.Outbox[oldKey] = expired
	state.Sessions["session-1"].Event.ExpiresAt = expired.Event.ExpiresAt
	added := attentionFixture()
	added.Identity = attentionDigest("added")
	added.Slug = "added"
	state.Sessions[added.Slug] = attentionBaseline{Identity: added.Identity, RootThread: added.RootThread, LeadTurn: added.Lead.CurrentTurnID, CompletedTurn: added.Lead.CurrentTurnID, ObservedAt: now.UnixMilli(), Eligible: true, Requests: []string{}}
	added.Lead.CurrentState = "waiting"
	added.Lead.LatestTurnStatus = "inProgress"
	added.Requests = []attentionRequest{{"added-request", "lead", "input"}}
	observations[added.Slug] = added
	reconcileNotificationState(&state, observations, false, now.Add(time.Second))
	if len(state.Outbox) != 512 || state.Outbox[notificationDeliveryKey(browserID, "added")].Event.Kind != "input" {
		t.Fatal("expired work prevented new admission")
	}
	stableID := state.Outbox[key].Event.ID
	reconcileNotificationState(&state, observations, false, now.Add(2*time.Second))
	if state.Outbox[key].Event.ID != stableID || len(state.Outbox) != 512 {
		t.Fatal("coalesced input replayed or disappeared")
	}
}

func TestNotificationsRegistrationOriginAndDelayedTest(t *testing.T) {
	n := testNotifications(t)
	sub := testPushSubscription(t)
	body, _ := json.Marshal(map[string]any{"subscription": sub, "expirationTime": nil})
	w := notificationRequest(n, http.MethodPost, "/api/browser-notifications/register", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("registration=%d %s", w.Code, w.Body)
	}
	var saved struct {
		RegistrationID string `json:"registrationId"`
	}
	json.Unmarshal(w.Body.Bytes(), &saved)
	if !messageDigestPattern.MatchString(saved.RegistrationID) {
		t.Fatal("missing opaque browser identity")
	}
	w = notificationRequest(n, http.MethodPost, "/api/browser-notifications/register", string(body))
	if w.Code != http.StatusOK || len(n.store.state.Browsers) != 1 {
		t.Fatal("repeat registration not idempotent")
	}
	before := time.Now()
	w = notificationRequest(n, http.MethodPost, "/api/browser-notifications/test", `{"registrationId":"`+saved.RegistrationID+`"}`)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	delivery := n.store.state.Outbox[notificationDeliveryKey(saved.RegistrationID, "test")]
	var reply struct {
		Test struct {
			ID        string `json:"id"`
			Identity  string `json:"identity"`
			Revision  uint64 `json:"revision"`
			ExpiresAt int64  `json:"expiresAt"`
		} `json:"test"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Test.ID != delivery.Event.ID || reply.Test.Identity != delivery.Event.Identity || reply.Test.Revision != delivery.Event.Revision || reply.Test.ExpiresAt != delivery.Event.ExpiresAt {
		t.Fatal("test reply does not describe its exact queued event")
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &fields)
	var descriptor map[string]json.RawMessage
	json.Unmarshal(fields["test"], &descriptor)
	if len(descriptor) != 4 || strings.Contains(w.Body.String(), sub.Endpoint) || strings.Contains(w.Body.String(), sub.Keys.Auth) || strings.Contains(w.Body.String(), sub.Keys.P256dh) {
		t.Fatal("test reply exposes more than its bounded descriptor")
	}
	if delivery.NextAt < before.Add(10*time.Second).UnixMilli() || delivery.Event.ExpiresAt > before.Add(61*time.Second).UnixMilli() {
		t.Fatal("test delay or expiry incorrect")
	}
	r := httptest.NewRequest(http.MethodPost, "https://workspace.example/api/browser-notifications/disable", strings.NewReader(`{"registrationId":"`+saved.RegistrationID+`"}`))
	r.Header.Set("Origin", "https://attacker.example")
	w = httptest.NewRecorder()
	n.server.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || len(n.store.state.Browsers) != 1 {
		t.Fatal("cross-origin mutation accepted")
	}
	w = notificationRequest(n, http.MethodPost, "/api/browser-notifications/disable", `{"registrationId":"`+saved.RegistrationID+`"}`)
	if w.Code != http.StatusOK || len(n.store.state.Browsers) != 0 || len(n.store.state.Outbox) != 0 {
		t.Fatal("disable retained pending delivery")
	}
}

func TestNotificationsDisableSelectorsAndScope(t *testing.T) {
	for _, selector := range []string{"registrationId", "endpointDigest"} {
		t.Run(selector, func(t *testing.T) {
			n := testNotifications(t)
			client := &mockPushClient{code: http.StatusCreated}
			n.client = client
			sub := testPushSubscription(t)
			target, foreign, unrelated := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
			n.store.mu.Lock()
			err := n.store.change(func(state *notificationState) error {
				for id, origin := range map[string]string{target: "https://workspace.example", foreign: "https://alias.example", unrelated: "https://workspace.example"} {
					copy := sub
					if id == unrelated {
						copy.Endpoint += "/unrelated"
					}
					state.Browsers[id] = browserRegistration{ID: id, Origin: origin, Subscription: copy}
					state.Outbox[id] = notificationDelivery{Browser: id}
				}
				return nil
			})
			n.store.mu.Unlock()
			if err != nil {
				t.Fatal("fixture persistence failed")
			}
			value := target
			if selector == "endpointDigest" {
				sum := sha256.Sum256([]byte(sub.Endpoint))
				value = hex.EncodeToString(sum[:])
			}
			body, _ := json.Marshal(map[string]string{selector: value})
			for i := 0; i < 2; i++ {
				w := notificationRequest(n, http.MethodPost, "/api/browser-notifications/disable", string(body))
				if w.Code != http.StatusOK || len(n.store.state.Browsers) != 2 || len(n.store.state.Outbox) != 2 {
					t.Fatal("scoped idempotent deletion failed")
				}
				if _, ok := n.store.state.Browsers[target]; ok {
					t.Fatal("target browser remains")
				}
				for _, id := range []string{foreign, unrelated} {
					if n.store.state.Browsers[id].ID != id || n.store.state.Outbox[id].Browser != id {
						t.Fatal("deletion crossed origin or registration boundary")
					}
				}
			}
			if client.calls != 0 {
				t.Fatal("deletion performed outbound work")
			}
		})
	}
}

func TestNotificationsDisableRejectsInvalidSelectorsAndOrigins(t *testing.T) {
	n := testNotifications(t)
	id := strings.Repeat("a", 64)
	for _, body := range []string{
		`{}`, `{"registrationId":null}`, `{"endpointDigest":null}`, `{"endpointDigest":true}`,
		`{"registrationId":""}`, `{"endpointDigest":"short"}`, `{"endpointDigest":"` + strings.Repeat("A", 64) + `"}`,
		`{"registrationId":"` + id + `","endpointDigest":"` + id + `"}`,
		`{"registrationId":"` + id + `","endpointDigest":null}`,
		`{"endpointDigest":"` + id + `","workspace":"another"}`,
	} {
		if w := notificationRequest(n, http.MethodPost, "/api/browser-notifications/disable", body); w.Code != http.StatusBadRequest {
			t.Fatal("invalid deletion selector accepted")
		}
	}
	body := `{"endpointDigest":"` + id + `"}`
	r := httptest.NewRequest(http.MethodPost, "https://workspace.example/api/browser-notifications/disable", strings.NewReader(body))
	r.Header.Set("Origin", "https://attacker.example")
	w := httptest.NewRecorder()
	n.server.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("cross-origin digest deletion accepted")
	}
	if w := notificationRequest(n, http.MethodGet, "/api/browser-notifications/disable", body); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("deletion accepted a read method")
	}
	// A never-registered, refused endpoint needs no admission or outbound call
	// to confirm absence. SHA-256 covers the exact string, including its path.
	sum := sha256.Sum256([]byte("https://refused.example/private-path?exact=value"))
	body = `{"endpointDigest":"` + hex.EncodeToString(sum[:]) + `"}`
	if w := notificationRequest(n, http.MethodPost, "/api/browser-notifications/disable", body); w.Code != http.StatusOK || len(n.store.state.Browsers) != 0 {
		t.Fatal("absent refused endpoint could not be removed")
	}
	n.server.hostProfile.target = "superseded-fixture"
	if w := notificationRequest(n, http.MethodPost, "/api/browser-notifications/disable", body); w.Code != http.StatusConflict {
		t.Fatal("superseded package accepted deletion")
	}
}

type mockPushClient struct {
	before  func()
	code    int
	err     error
	calls   int
	headers http.Header
}

func (m *mockPushClient) Do(r *http.Request) (*http.Response, error) {
	m.calls++
	if m.before != nil {
		m.before()
	}
	m.headers = r.Header.Clone()
	if m.err != nil {
		return nil, m.err
	}
	return &http.Response{StatusCode: m.code, Header: http.Header{"Retry-After": []string{"30"}}, Body: io.NopCloser(strings.NewReader("private provider response"))}, nil
}

func TestNotificationsSenderNoPageRestartAndProviderOutcomes(t *testing.T) {
	for _, code := range []int{201, 410, 429, 500, 400} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			n := testNotifications(t)
			now := time.Now()
			id := strings.Repeat("b", 64)
			browser := browserRegistration{ID: id, Origin: "https://workspace.example", Subscription: testPushSubscription(t)}
			event := attentionEvent{Version: 1, ID: strings.Repeat("c", 64), Identity: "test-" + id, Revision: 1, Kind: "test", Route: "/", ExpiresAt: now.Add(time.Minute).UnixMilli()}
			key := notificationDeliveryKey(id, "test")
			n.store.mu.Lock()
			err := n.store.change(func(state *notificationState) error {
				state.Browsers[id] = browser
				state.Outbox[key] = notificationDelivery{Browser: id, Event: event}
				return nil
			})
			n.store.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			// Restart between durable enqueue and send; there are no browser clients.
			n.store, err = newNotificationStore(n.server.operationStore)
			if err != nil {
				t.Fatal(err)
			}
			client := &mockPushClient{code: code}
			n.client = client
			n.sendNext(context.Background(), now)
			if client.calls != 1 || client.headers.Get("Authorization") == "" || client.headers.Get("Content-Encoding") != "aes128gcm" || client.headers.Get("Topic") == "" {
				t.Fatal("standard Web Push transport missing")
			}
			pending, found := n.store.state.Outbox[key]
			result := n.store.state.Browsers[id]
			if code == 429 || code == 500 {
				if !found || pending.Event.ID != event.ID || pending.NextAt < now.Add(30*time.Second).UnixMilli() {
					t.Fatal("retry lost identity or backoff")
				}
			} else if found {
				t.Fatal("terminal delivery retained")
			}
			if (code == 410) != result.Disabled {
				t.Fatal("expired subscription retirement incorrect")
			}
		})
	}
}

func TestNotificationsPushValidationAndTransportGuards(t *testing.T) {
	for _, endpoint := range []string{"https://fcm.googleapis.com/push", "https://updates.push.services.mozilla.com/push", "https://jmt17.google.com/push", "https://jmt17.google.com:443/push"} {
		if _, err := validatePushEndpoint(endpoint); err != nil {
			t.Fatal("exact supported provider rejected")
		}
	}
	for _, endpoint := range []string{"http://fcm.googleapis.com/push", "https://fcm.googleapis.com.evil.test/push", "https://jmt17.google.com.evil.test/push", "https://not-jmt17.google.com/push", "https://jmt18.google.com/push", "http://jmt17.google.com/push", "https://jmt17.google.com:444/push", "https://127.0.0.1/push", "https://user@fcm.googleapis.com/push", "https://fcm.googleapis.com:444/push", "https://updates.push.services.mozilla.com/push#fragment"} {
		if _, err := validatePushEndpoint(endpoint); err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "100.64.0.1", "192.0.2.1", "::1", "::ffff:10.0.0.1", "fc00::1", "2001:db8::1"} {
		if publicPushIP(netip.MustParseAddr(ip)) {
			t.Fatalf("accepted non-public %s", ip)
		}
	}
	if !publicPushIP(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public destination rejected")
	}
	sub := testPushSubscription(t)
	sub.Keys.Auth = "bad"
	if validatePushSubscription(sub, 0, time.Now()) == nil {
		t.Fatal("malformed auth accepted")
	}
	client := guardedPushClient()
	if client.Transport.(*http.Transport).Proxy != nil || client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("proxy/redirect bypass present")
	}
	_, err := client.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "127.0.0.1:443")
	if err == nil {
		t.Fatal("unchecked dial accepted")
	}
}

type attentionObserverFixture struct {
	*testActivityObserver
	prompts []codex.Prompt
}

func (o *attentionObserverFixture) Prompts(string) []codex.Prompt { return o.prompts }

func TestNotificationsObserverOutboxWithoutBrowserConnections(t *testing.T) {
	n := testNotifications(t)
	prepareInteractiveConversation(t, n.server, "example")
	observer := &attentionObserverFixture{testActivityObserver: &testActivityObserver{}}
	n.server.activity = &activityMonitor{server: n.server, observer: observer, snapshots: make(map[string]activityCachedSnapshot)}
	now := time.Now()
	set := func(turn, status, state, wait string) {
		n.server.activity.snapshots["thread-1"] = activityCachedSnapshot{at: now, value: codex.ActivitySnapshot{
			ThreadID: "thread-1", CurrentTurnID: turn, LatestTurnStatus: status, CurrentState: state, WaitReason: wait, ObservedAtMS: now.UnixMilli()}}
	}
	set("old", "completed", "idle", "")
	id := strings.Repeat("b", 64)
	n.store.state.Browsers[id] = browserRegistration{ID: id, Origin: "https://workspace.example", Subscription: testPushSubscription(t)}
	if !n.reconcile(now) || len(n.store.state.Outbox) != 0 {
		t.Fatal("first no-tab baseline emitted historical idle")
	}
	now = now.Add(time.Second)
	set("new", "inProgress", "working", "")
	n.reconcile(now)
	now = now.Add(time.Second)
	set("new", "inProgress", "waiting", "userInput")
	observer.prompts = []codex.Prompt{{ThreadID: "thread-1", TurnID: "new", ItemID: "item", Method: "item/tool/requestUserInput", Kind: "userInput", IsBlocking: false}}
	n.reconcile(now)
	if len(n.store.state.Outbox) != 0 {
		t.Fatal("optional input alerted")
	}
	observer.prompts[0].IsBlocking = true
	now = now.Add(time.Second)
	set("new", "inProgress", "waiting", "userInput")
	n.reconcile(now)
	key := notificationDeliveryKey(id, "example")
	delivery, found := n.store.state.Outbox[key]
	if !found || delivery.Event.Kind != "input" {
		t.Fatal("no-tab observer did not durably enqueue input")
	}
	reopened, err := newNotificationStore(n.server.operationStore)
	if err != nil {
		t.Fatal(err)
	}
	n.store = reopened
	n.reconcile(now)
	if len(n.store.state.Outbox) != 1 || n.store.state.Outbox[key].Event.ID != delivery.Event.ID {
		t.Fatal("restart lost or duplicated event")
	}
	client := &mockPushClient{code: 201}
	n.client = client
	n.store.presence[key] = notificationPresence{Identity: delivery.Event.Identity, Until: now.Add(time.Second)}
	n.sendNext(context.Background(), now)
	if client.calls != 0 {
		t.Fatal("fresh foreground lease did not defer")
	}
	n.store.presence[key] = notificationPresence{Identity: delivery.Event.Identity, EventID: delivery.Event.ID, Until: now.Add(time.Second)}
	n.sendNext(context.Background(), now)
	if client.calls != 0 || len(n.store.state.Outbox) != 0 {
		t.Fatal("confirmed focus did not suppress")
	}
	observer.prompts = nil
	now = now.Add(time.Second)
	set("new", "completed", "idle", "")
	n.reconcile(now)
	if n.store.state.Outbox[key].Event.Kind != "done" {
		t.Fatal("successful completed response not emitted")
	}
	now = now.Add(time.Second)
	set("next", "inProgress", "working", "")
	n.reconcile(now)
	if len(n.store.state.Outbox) != 0 {
		t.Fatal("newer turn did not cancel unsent completion")
	}
	now = now.Add(time.Second)
	set("next", "completed", "idle", "")
	n.reconcile(now)
	n.store.state.Preferences["example"] = notificationPreference{Identity: delivery.Event.Identity, Enabled: false}
	n.reconcile(now)
	if len(n.store.state.Outbox) != 0 {
		t.Fatal("mute retained unsent completion")
	}
	n.store.state.Preferences["example"] = notificationPreference{Identity: delivery.Event.Identity, Enabled: true}
	n.reconcile(now)
	if len(n.store.state.Outbox) != 0 {
		t.Fatal("unmute replayed idle")
	}
	summary, err := session.Find(n.server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	summary.Archived = true
	if n.server.observeAttention(*summary, now).Eligible {
		t.Fatal("archived session eligible")
	}
}

func TestNotificationsFreshEpochDoesNotReplayDisabledPeriod(t *testing.T) {
	n := testNotifications(t)
	subscription := testPushSubscription(t)
	body, _ := json.Marshal(map[string]any{"subscription": subscription})
	register := func(fresh bool) string {
		t.Helper()
		input := body
		if fresh {
			input, _ = json.Marshal(map[string]any{"subscription": subscription, "requireFresh": true})
		}
		w := notificationRequest(n, http.MethodPost, "/api/browser-notifications/register", string(input))
		if w.Code != http.StatusOK {
			t.Fatalf("register status %d", w.Code)
		}
		var reply struct {
			RegistrationID string `json:"registrationId"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		return reply.RegistrationID
	}
	old := register(false)
	w := notificationRequest(n, http.MethodGet, "/api/browser-notifications?registrationId="+old, "")
	var capability struct {
		DeliveryEpoch int `json:"deliveryEpoch"`
	}
	json.Unmarshal(w.Body.Bytes(), &capability)
	if capability.DeliveryEpoch != 1 {
		t.Fatal("missing negotiated epoch capability")
	}
	notificationRequest(n, http.MethodPost, "/api/browser-notifications/test", `{"registrationId":"`+old+`"}`)
	if register(false) != old || len(n.store.state.Outbox) != 1 {
		t.Fatal("continuous registration reset pending delivery")
	}
	freshBody, _ := json.Marshal(map[string]any{"subscription": subscription, "requireFresh": true})
	w = notificationRequest(n, http.MethodPost, "/api/browser-notifications/register", string(freshBody))
	if w.Code != http.StatusConflict || len(n.store.state.Browsers) != 1 || len(n.store.state.Outbox) != 1 {
		t.Fatal("fresh activation adopted an existing endpoint")
	}
	other := strings.Repeat("e", 64)
	otherSub := subscription
	otherSub.Endpoint += "-other"
	now := time.Now()
	observation := attentionFixture()
	observation.Lead.LatestTurnStatus = "inProgress"
	observation.Lead.CurrentState = "working"
	observations := map[string]attentionObservation{observation.Slug: observation}
	n.store.mu.Lock()
	err := n.store.change(func(state *notificationState) error {
		state.Browsers[other] = browserRegistration{ID: other, Origin: "https://workspace.example", Subscription: otherSub}
		reconcileNotificationState(state, observations, false, now)
		return nil
	})
	n.store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if w := notificationRequest(n, http.MethodPost, "/api/browser-notifications/disable", `{"registrationId":"`+old+`"}`); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	observation.Lead.LatestTurnStatus = "completed"
	observation.Lead.CurrentState = "idle"
	observations[observation.Slug] = observation
	n.store.mu.Lock()
	err = n.store.change(func(state *notificationState) error {
		reconcileNotificationState(state, observations, false, now.Add(time.Second))
		return nil
	})
	n.store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(n.store.state.Outbox) != 1 || n.store.state.Outbox[notificationDeliveryKey(other, observation.Slug)].Browser != other {
		t.Fatal("off period affected other browser or retained disabled work")
	}
	fresh := register(true)
	if fresh == old || len(n.store.state.Outbox) != 1 {
		t.Fatal("reenable replayed a historical event")
	}
	n.store.mu.Lock()
	err = n.store.change(func(state *notificationState) error {
		reconcileNotificationState(state, observations, false, now.Add(2*time.Second))
		observation.Lead.CurrentTurnID = "next"
		observation.Lead.LatestTurnStatus = "inProgress"
		observation.Lead.CurrentState = "working"
		observations[observation.Slug] = observation
		reconcileNotificationState(state, observations, false, now.Add(3*time.Second))
		observation.Lead.LatestTurnStatus = "completed"
		observation.Lead.CurrentState = "idle"
		observations[observation.Slug] = observation
		reconcileNotificationState(state, observations, false, now.Add(4*time.Second))
		return nil
	})
	n.store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	delivery := n.store.state.Outbox[notificationDeliveryKey(fresh, observation.Slug)]
	if delivery.Browser != fresh || len(n.store.state.Outbox) != 2 {
		t.Fatal("future event did not reach both current registrations")
	}
	var payload map[string]any
	json.Unmarshal(notificationPayload(delivery), &payload)
	if payload["registrationId"] != fresh || payload["id"] != delivery.Event.ID || payload["version"] != float64(1) {
		t.Fatal("transport epoch changed event contract")
	}
	persisted, _ := json.Marshal(n.store.state)
	if n.store.state.Schema != 1 || strings.Contains(string(persisted), `"registrationId"`) {
		t.Fatal("transport epoch leaked into persisted state")
	}
}

func TestNotificationsDisableDuringProviderSendCannotRecreateRegistrationOrJob(t *testing.T) {
	for _, code := range []int{201, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			n := testNotifications(t)
			sub := testPushSubscription(t)
			body, _ := json.Marshal(map[string]any{"subscription": sub})
			w := notificationRequest(n, http.MethodPost, "/api/browser-notifications/register", string(body))
			var reply struct {
				RegistrationID string `json:"registrationId"`
			}
			json.Unmarshal(w.Body.Bytes(), &reply)
			notificationRequest(n, http.MethodPost, "/api/browser-notifications/test", `{"registrationId":"`+reply.RegistrationID+`"}`)
			client := &mockPushClient{code: code, before: func() {
				w := notificationRequest(n, http.MethodPost, "/api/browser-notifications/disable", `{"registrationId":"`+reply.RegistrationID+`"}`)
				if w.Code != http.StatusOK {
					t.Fatal(w.Code)
				}
			}}
			n.client = client
			n.sendNext(context.Background(), time.Now().Add(11*time.Second))
			if client.calls != 1 || len(n.store.state.Browsers) != 0 || len(n.store.state.Outbox) != 0 {
				t.Fatal("provider completion resurrected disabled delivery")
			}
		})
	}
}
