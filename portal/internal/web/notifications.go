package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type notificationService struct {
	server *Server
	store  *notificationStore
	client webpush.HTTPClient
}

func (s *Server) initNotifications() {
	if !s.config.ObserveActivity {
		return
	}
	store, err := newNotificationStore(s.operationStore)
	if err != nil {
		s.config.Logger.Print("Browser notification storage is unavailable; delivery is disabled.")
		return
	}
	s.notifications = &notificationService{server: s, store: store, client: guardedPushClient()}
}

func (s *Server) startNotifications() {
	if s.notifications == nil {
		return
	}
	s.operationWG.Add(1)
	go s.notifications.run(s.operationContext)
}

func (n *notificationService) run(ctx context.Context) {
	defer n.server.operationWG.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if n.reconcile(time.Now()) {
			n.sendNext(ctx, time.Now())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (n *notificationService) disabled() bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(n.store.path), "browser-notifications.disabled"))
	return !errors.Is(err, os.ErrNotExist)
}

func (n *notificationService) reconcile(now time.Time) bool {
	if err := n.server.requireCurrentHostProfile(); err != nil {
		return false
	}
	summaries, err := session.List(n.server.config.Workspace)
	if err != nil {
		return false
	}
	observations := make(map[string]attentionObservation, len(summaries))
	for _, summary := range summaries {
		observations[summary.Slug] = n.server.observeAttention(summary, now)
	}
	n.store.mu.Lock()
	defer n.store.mu.Unlock()
	if n.store.failed {
		return false
	}
	for key, presence := range n.store.presence {
		if !presence.Until.After(now) {
			delete(n.store.presence, key)
		}
	}
	disabled := n.disabled()
	err = n.store.change(func(state *notificationState) error {
		reconcileNotificationState(state, observations, disabled, now)
		return nil
	})
	return err == nil && !disabled
}

// Reduce every baseline before pruning and admitting delivery. Both changes are
// committed together; a full outbox must still replace its existing session key.
func reconcileNotificationState(state *notificationState, observations map[string]attentionObservation, disabled bool, now time.Time) {
	for id, browser := range state.Browsers {
		if browser.ExpiresAt != 0 && browser.ExpiresAt <= now.UnixMilli() {
			browser.Disabled = true
			browser.Result = "expired"
			state.Browsers[id] = browser
		}
	}
	for slug, baseline := range state.Sessions {
		if _, found := observations[slug]; !found {
			baseline.Eligible = false
			baseline.Event = nil
			state.Sessions[slug] = baseline
		}
	}
	emitted := make(map[string]attentionEvent)
	for slug, observation := range observations {
		if observation.Identity == "" {
			continue
		}
		baseline, emit := reduceAttention(state.Sessions[slug], observation, now)
		state.Sessions[slug] = baseline
		if emit && !disabled && notificationEnabled(state, slug, observation.Identity) {
			emitted[slug] = *baseline.Event
		}
	}
	for key, delivery := range state.Outbox {
		browser, found := state.Browsers[delivery.Browser]
		baseline := state.Sessions[delivery.Event.Slug]
		if disabled || !found || browser.Disabled || delivery.Event.ExpiresAt <= now.UnixMilli() ||
			(delivery.Event.Kind != "test" && (!baseline.Eligible || baseline.Event == nil || baseline.Event.ID != delivery.Event.ID ||
				baseline.Identity != delivery.Event.Identity || !notificationEnabled(state, delivery.Event.Slug, delivery.Event.Identity))) {
			delete(state.Outbox, key)
		}
	}
	for slug, event := range emitted {
		for id, browser := range state.Browsers {
			if browser.Disabled {
				continue
			}
			key := notificationDeliveryKey(id, slug)
			if _, exists := state.Outbox[key]; !exists && len(state.Outbox) >= 512 {
				continue
			}
			state.Outbox[key] = notificationDelivery{Browser: id, Event: event, NextAt: now.UnixMilli()}
		}
	}
}

func (n *notificationService) sendNext(ctx context.Context, now time.Time) {
	n.store.mu.Lock()
	if n.store.failed || n.disabled() {
		n.store.mu.Unlock()
		return
	}
	var key string
	var delivery notificationDelivery
	var browser browserRegistration
	for k, d := range n.store.state.Outbox {
		if d.NextAt > now.UnixMilli() {
			continue
		}
		p := n.store.presence[notificationDeliveryKey(d.Browser, d.Event.Slug)]
		if p.Identity == d.Event.Identity && p.Until.After(now) {
			if p.EventID == d.Event.ID {
				_ = n.store.change(func(state *notificationState) error { delete(state.Outbox, k); return nil })
				continue
			}
			continue
		}
		key, delivery, browser = k, d, n.store.state.Browsers[d.Browser]
		break
	}
	private, public := n.store.state.PrivateKey, n.store.state.PublicKey
	n.store.mu.Unlock()
	if key == "" {
		return
	}
	if delivery.Event.Kind != "test" {
		summary, err := session.Find(n.server.config.Workspace, delivery.Event.Slug)
		if err != nil {
			return
		}
		o := n.server.observeAttention(*summary, now)
		if !o.Eligible || !o.Fresh || o.Identity != delivery.Event.Identity || !attentionRelevant(delivery.Event, o) {
			return
		}
	}
	if browser.Disabled || validatePushSubscription(browser.Subscription, browser.ExpiresAt, now) != nil {
		return
	}
	data, _ := json.Marshal(delivery.Event)
	requestCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	response, err := webpush.SendNotificationWithContext(requestCtx, data, &browser.Subscription, &webpush.Options{
		HTTPClient: n.client, Subscriber: n.server.config.BaseURL, VAPIDPrivateKey: private, VAPIDPublicKey: public,
		TTL: int((delivery.Event.ExpiresAt - now.UnixMilli() + 999) / 1000), Topic: attentionDigest(delivery.Event.Identity)[:32], Urgency: webpush.UrgencyNormal})
	cancel()
	code, retryAfter := 0, ""
	if response != nil {
		code, retryAfter = response.StatusCode, response.Header.Get("Retry-After")
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
	}
	n.store.mu.Lock()
	defer n.store.mu.Unlock()
	_ = n.store.change(func(state *notificationState) error {
		current, found := state.Outbox[key]
		if !found || current.Event.ID != delivery.Event.ID {
			return nil
		}
		b, found := state.Browsers[delivery.Browser]
		if !found {
			delete(state.Outbox, key)
			return nil
		}
		if err == nil && code >= 200 && code < 300 {
			delete(state.Outbox, key)
			b.Result = "provider_accepted"
		} else if code == 404 || code == 410 {
			delete(state.Outbox, key)
			b.Disabled = true
			b.Result = "expired"
		} else if err != nil || code == 429 || code >= 500 {
			current.Attempt++
			if current.Attempt >= 6 {
				delete(state.Outbox, key)
				b.Result = "delivery_failed"
			} else {
				delay := notificationRetryDelay(current.Attempt, retryAfter, now)
				current.NextAt = now.Add(delay).UnixMilli()
				state.Outbox[key] = current
				b.Result = "retrying"
			}
		} else {
			delete(state.Outbox, key)
			b.Result = "provider_refused"
		}
		state.Browsers[b.ID] = b
		return nil
	})
}

func attentionRelevant(event attentionEvent, observation attentionObservation) bool {
	if event.Kind != "done" {
		for _, request := range observation.Requests {
			if attentionDigest(observation.Identity, request.Key) == event.ID {
				return true
			}
		}
		return false
	}
	if len(observation.Requests) != 0 || observation.Lead.CurrentState != "idle" || observation.Lead.LatestTurnStatus != "completed" ||
		attentionDigest(observation.Identity, "done", observation.Lead.CurrentTurnID) != event.ID {
		return false
	}
	for _, member := range observation.Members {
		if member.CurrentState != "idle" {
			return false
		}
	}
	return true
}

func notificationRetryDelay(attempt int, retryAfter string, now time.Time) time.Duration {
	delay := time.Duration(1<<min(attempt, 6)) * 5 * time.Second
	if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds > 0 {
		delay = max(delay, time.Duration(min(seconds, 300))*time.Second)
	} else if at, err := http.ParseTime(retryAfter); err == nil {
		delay = max(delay, at.Sub(now))
	}
	return min(delay, 5*time.Minute)
}

func (s *Server) notificationOrigin(r *http.Request) string {
	for _, origin := range s.trustedOrigins {
		if origin == "https://"+r.Host {
			return origin
		}
	}
	return ""
}

func (s *Server) browserNotificationsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPost {
		if err := s.requireCurrentHostProfile(); err != nil {
			s.writeError(w, r, http.StatusConflict, "Workspace package changed. Reload before saving.")
			return
		}
	}
	origin := s.notificationOrigin(r)
	if origin == "" || (r.Method != http.MethodGet && r.Header.Get("Origin") != origin) {
		s.writeError(w, r, http.StatusForbidden, "Notification origin is invalid.")
		return
	}
	if s.notifications == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "Browser notifications are unavailable.")
		return
	}
	n := s.notifications
	n.store.mu.Lock()
	defer n.store.mu.Unlock()
	if n.store.failed {
		s.writeError(w, r, http.StatusServiceUnavailable, "Notification storage is unavailable.")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/browser-notifications" {
		id := r.URL.Query().Get("registrationId")
		browser, found := n.store.state.Browsers[id]
		registered := found && browser.Origin == origin && !browser.Disabled
		result := ""
		if found && browser.Origin == origin {
			result = browser.Result
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"version": 1, "publicKey": n.store.state.PublicKey, "registered": registered, "result": result, "available": !n.disabled()})
		return
	}
	if r.URL.Path == "/api/browser-notifications/disable" {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Raw fields distinguish omission from explicit null or an empty selector.
		var input struct {
			RegistrationID json.RawMessage `json:"registrationId"`
			EndpointDigest json.RawMessage `json:"endpointDigest"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		if !s.decodeJSON(w, r, &input) {
			return
		}
		var id, digest string
		if (len(input.RegistrationID) == 0) == (len(input.EndpointDigest) == 0) ||
			(len(input.RegistrationID) != 0 && (json.Unmarshal(input.RegistrationID, &id) != nil || !messageDigestPattern.MatchString(id))) ||
			(len(input.EndpointDigest) != 0 && (json.Unmarshal(input.EndpointDigest, &digest) != nil || !messageDigestPattern.MatchString(digest))) {
			s.writeError(w, r, http.StatusBadRequest, "Invalid notification action.")
			return
		}
		err := n.store.change(func(state *notificationState) error {
			for browserID, browser := range state.Browsers {
				if browser.Origin != origin {
					continue
				}
				sum := sha256.Sum256([]byte(browser.Subscription.Endpoint))
				if (id != "" && browserID != id) || (digest != "" && hex.EncodeToString(sum[:]) != digest) {
					continue
				}
				delete(state.Browsers, browserID)
				for key, delivery := range state.Outbox {
					if delivery.Browser == browserID {
						delete(state.Outbox, key)
					}
				}
			}
			return nil
		})
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	var input struct {
		RegistrationID string               `json:"registrationId"`
		Subscription   webpush.Subscription `json:"subscription"`
		ExpirationTime *int64               `json:"expirationTime"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if !s.decodeJSON(w, r, &input) {
		return
	}
	var outputID string
	err := n.store.change(func(state *notificationState) error {
		switch r.URL.Path {
		case "/api/browser-notifications/register":
			if r.Method != http.MethodPost {
				return errors.New("Invalid notification action.")
			}
			expires := int64(0)
			if input.ExpirationTime != nil {
				expires = *input.ExpirationTime
			}
			if err := validatePushSubscription(input.Subscription, expires, time.Now()); err != nil {
				return err
			}
			for id, browser := range state.Browsers {
				if browser.Subscription.Endpoint == input.Subscription.Endpoint {
					if browser.Origin != origin {
						return errors.New("This subscription belongs to another portal origin.")
					}
					outputID = id
				}
			}
			if outputID == "" {
				var bytes [32]byte
				if _, err := rand.Read(bytes[:]); err != nil {
					return errors.New("Registration could not be created.")
				}
				outputID = hex.EncodeToString(bytes[:])
			}
			state.Browsers[outputID] = browserRegistration{ID: outputID, Origin: origin, Subscription: input.Subscription, ExpiresAt: expires}
		case "/api/browser-notifications/test":
			browser, found := state.Browsers[input.RegistrationID]
			if !found || browser.Origin != origin {
				return errors.New("Browser registration is unavailable. Enable notifications again.")
			}
			if r.Method == http.MethodPost && !browser.Disabled && !n.disabled() {
				now := time.Now()
				event := attentionEvent{Version: 1, ID: attentionDigest(browser.ID, now.String()), Identity: "test-" + browser.ID, Revision: uint64(now.UnixMilli()), Kind: "test", Route: "/", ExpiresAt: now.Add(time.Minute).UnixMilli()}
				state.Outbox[notificationDeliveryKey(browser.ID, "test")] = notificationDelivery{Browser: browser.ID, Event: event, NextAt: now.Add(10 * time.Second).UnixMilli()}
			} else {
				return errors.New("Browser notifications are disabled.")
			}
		default:
			return errors.New("Invalid notification action.")
		}
		return nil
	})
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "registrationId": outputID})
}

func (s *Server) attentionAPI(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPost {
		if err := s.requireCurrentHostProfile(); err != nil {
			s.writeError(w, r, http.StatusConflict, "Workspace package changed. Reload before saving.")
			return
		}
	}
	summary, err := session.Find(s.config.Workspace, slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	identity, err := session.ObservationIdentity(s.config.Workspace, slug, filepath.Join(s.config.Workspace, summary.Root, slug), summary.Codex.ThreadID)
	if err != nil || s.notifications == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "Session attention is unavailable.")
		return
	}
	n := s.notifications
	origin := s.notificationOrigin(r)
	if origin == "" || (r.Method != http.MethodGet && r.Header.Get("Origin") != origin) {
		s.writeError(w, r, http.StatusForbidden, "Notification origin is invalid.")
		return
	}
	n.store.mu.Lock()
	defer n.store.mu.Unlock()
	if n.store.failed {
		s.writeError(w, r, http.StatusServiceUnavailable, "Notification storage is unavailable.")
		return
	}
	if r.Method == http.MethodPost {
		var input struct {
			Identity       string `json:"identity"`
			Enabled        *bool  `json:"enabled"`
			RegistrationID string `json:"registrationId"`
			EventID        string `json:"eventId"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2048)
		if !s.decodeJSON(w, r, &input) {
			return
		}
		if input.Identity != identity {
			s.writeError(w, r, http.StatusConflict, "Session identity changed. Reload before saving.")
			return
		}
		if input.Enabled != nil {
			err = n.store.change(func(state *notificationState) error {
				state.Preferences[slug] = notificationPreference{Identity: identity, Enabled: *input.Enabled}
				for key, delivery := range state.Outbox {
					if delivery.Event.Identity == identity {
						delete(state.Outbox, key)
					}
				}
				return nil
			})
		} else {
			browser, found := n.store.state.Browsers[input.RegistrationID]
			if !found || browser.Origin != origin || browser.Disabled {
				s.writeError(w, r, http.StatusConflict, "Browser registration is unavailable.")
				return
			}
			// A short presence lease is presentation only, never shared unread state.
			if len(n.store.presence) < 512 || n.store.presence[notificationDeliveryKey(browser.ID, slug)].Identity != "" {
				n.store.presence[notificationDeliveryKey(browser.ID, slug)] = notificationPresence{Identity: identity, EventID: input.EventID, Until: time.Now().Add(12 * time.Second)}
			}
		}
		if err != nil {
			s.writeError(w, r, http.StatusServiceUnavailable, "Notification settings could not be saved.")
			return
		}
	}
	baseline := n.store.state.Sessions[slug]
	fresh := baseline.Identity == identity && baseline.Eligible && baseline.Fresh && time.Now().UnixMilli()-baseline.ObservedAt < 40000
	var event *attentionEvent
	if fresh {
		event = baseline.Event
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"version": 1, "identity": identity, "workspace": attentionDigest(s.config.Workspace), "rootThread": summary.Codex.ThreadID,
		"enabled": notificationEnabled(&n.store.state, slug, identity), "fresh": fresh, "event": event, "revision": baseline.Revision})
}

// Capture creation preferences before any helper makes the session observable.
// The receipt remains authoritative until its preference is bound to the exact
// destination incarnation, including retries after a lost helper response.
func (s *Server) bindCreationNotificationPreference(receipt creationReceipt) error {
	if s.notifications == nil {
		if receipt.Request.BrowserNotificationsEnabled != nil && !*receipt.Request.BrowserNotificationsEnabled {
			return errors.New("Notification preference cannot be saved.")
		}
		return nil
	}
	summary, err := session.Find(s.config.Workspace, receipt.Request.Slug)
	if err != nil {
		return err
	}
	identity, err := session.ObservationIdentity(s.config.Workspace, summary.Slug, filepath.Join(s.config.Workspace, summary.Root, summary.Slug), summary.Codex.ThreadID)
	if err != nil {
		return err
	}
	store := s.notifications.store
	store.mu.Lock()
	defer store.mu.Unlock()
	if preference, found := store.state.Preferences[summary.Slug]; found && preference.Identity == identity {
		return nil
	}
	enabled := receipt.Request.BrowserNotificationsEnabled == nil || *receipt.Request.BrowserNotificationsEnabled
	return store.change(func(state *notificationState) error {
		state.Preferences[summary.Slug] = notificationPreference{Identity: identity, Enabled: enabled}
		return nil
	})
}
