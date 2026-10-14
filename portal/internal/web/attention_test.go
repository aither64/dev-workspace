package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func attentionFixture() attentionObservation {
	return attentionObservation{Identity: strings.Repeat("a", 64), Slug: "example", RootThread: "lead", Fresh: true, Eligible: true,
		Lead: codex.ActivitySnapshot{ThreadID: "lead", CurrentTurnID: "turn-1", LatestTurnStatus: "completed", CurrentState: "idle"}}
}

func TestAttentionBaselineDeferredCompletionAndNewerTurn(t *testing.T) {
	now := time.Now()
	o := attentionFixture()
	b, emit := reduceAttention(attentionBaseline{}, o, now)
	if emit || b.Event != nil {
		t.Fatal("first idle observation manufactured completion")
	}
	o.Lead.CurrentTurnID = "turn-2"
	o.Lead.LatestTurnStatus = "inProgress"
	o.Lead.CurrentState = "working"
	b, emit = reduceAttention(b, o, now.Add(time.Second))
	if emit {
		t.Fatal("active turn notified")
	}
	o.Lead.LatestTurnStatus = "completed"
	o.Lead.CurrentState = "idle"
	for _, state := range []string{"working", "waiting", "unclassified"} {
		o.Members = []codex.ActivitySnapshot{{CurrentState: state}}
		b, emit = reduceAttention(b, o, now.Add(2*time.Second))
		if emit {
			t.Fatalf("peer state %s did not defer", state)
		}
	}
	o.Members = []codex.ActivitySnapshot{{CurrentState: "idle"}}
	b, emit = reduceAttention(b, o, now.Add(3*time.Second))
	if !emit || b.Event.Kind != "done" {
		t.Fatal("settled team did not emit lead completion")
	}
	id := b.Event.ID
	b, emit = reduceAttention(b, o, now.Add(4*time.Second))
	if emit || b.Event.ID != id {
		t.Fatal("lead completion replayed")
	}
	o.Lead.CurrentTurnID = "turn-3"
	o.Lead.CurrentState = "working"
	o.Lead.LatestTurnStatus = "inProgress"
	b, emit = reduceAttention(b, o, now.Add(5*time.Second))
	if emit || b.Event != nil {
		t.Fatal("new activity retained old completion")
	}
	for _, state := range []string{"failed", "interrupted"} {
		o.Lead.LatestTurnStatus = state
		o.Lead.CurrentState = "idle"
		b, emit = reduceAttention(b, o, now.Add(6*time.Second))
		if emit || b.Event != nil {
			t.Fatalf("%s received a success event", state)
		}
	}
}

func TestAttentionInputPriorityResolutionAndContinuity(t *testing.T) {
	now := time.Now()
	o := attentionFixture()
	o.Lead.LatestTurnStatus = "inProgress"
	o.Lead.CurrentState = "working"
	b, _ := reduceAttention(attentionBaseline{}, o, now)
	o.Requests = []attentionRequest{{"request-one", "implementer0", "input"}}
	o.Members = []codex.ActivitySnapshot{{CurrentState: "waiting"}}
	b, emit := reduceAttention(b, o, now.Add(time.Second))
	if !emit || b.Event.Kind != "input" || b.Event.Member != "implementer0" || b.Event.Route != "/example/?member=implementer0#codex" {
		t.Fatal("blocking member input missing")
	}
	b, emit = reduceAttention(b, o, now.Add(2*time.Second))
	if emit {
		t.Fatal("input replayed")
	}
	o.Requests = nil
	o.Lead.LatestTurnStatus = "completed"
	o.Lead.CurrentState = "idle"
	o.Members = nil
	b, emit = reduceAttention(b, o, now.Add(3*time.Second))
	if !emit || b.Event.Kind != "done" {
		t.Fatal("resolved request did not allow completion")
	}
	o.Fresh = false
	b, emit = reduceAttention(b, o, now.Add(4*time.Second))
	if emit || b.Fresh {
		t.Fatal("unknown state claimed fresh attention")
	}
	o.Fresh = true
	b, emit = reduceAttention(b, o, now.Add(5*time.Second))
	if emit {
		t.Fatal("lost continuity replayed idle")
	}
	o.Identity = strings.Repeat("b", 64)
	b, emit = reduceAttention(b, o, now.Add(6*time.Second))
	if emit || b.Event != nil {
		t.Fatal("new incarnation inherited completion")
	}
}

func TestAttentionMuteSurvivesTrackingMoveButNotSlugReuse(t *testing.T) {
	workspace := t.TempDir()
	slug := "example"
	directory := filepath.Join(workspace, "work", slug)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	id, err := session.ObservationIdentity(workspace, slug, directory, "thread")
	if err != nil {
		t.Fatal(err)
	}
	state := notificationState{Preferences: map[string]notificationPreference{slug: {Identity: id, Enabled: false}}}
	archive := filepath.Join(workspace, "archive", slug)
	os.MkdirAll(filepath.Dir(archive), 0700)
	if err := os.Rename(directory, archive); err != nil {
		t.Fatal(err)
	}
	archived, err := session.ObservationIdentity(workspace, slug, archive, "thread")
	if err != nil {
		t.Fatal(err)
	}
	if archived != id || notificationEnabled(&state, slug, archived) {
		t.Fatal("archive lost mute")
	}
	if err := os.Rename(archive, directory); err != nil {
		t.Fatal(err)
	}
	revived, _ := session.ObservationIdentity(workspace, slug, directory, "thread")
	if notificationEnabled(&state, slug, revived) {
		t.Fatal("revive lost mute")
	}
	other, _ := session.ObservationIdentity(workspace, slug, directory, "replacement-thread")
	if !notificationEnabled(&state, slug, other) {
		t.Fatal("replacement inherited mute")
	}
}

func TestAttentionCreationPreferenceEquality(t *testing.T) {
	no, yes := false, true
	if !sameCreationRequest(creationRequest{}, creationRequest{BrowserNotificationsEnabled: &yes}) {
		t.Fatal("old omitted default incompatible")
	}
	if sameCreationRequest(creationRequest{}, creationRequest{BrowserNotificationsEnabled: &no}) {
		t.Fatal("false treated as absent")
	}
	other := false
	if !sameCreationRequest(creationRequest{BrowserNotificationsEnabled: &no}, creationRequest{BrowserNotificationsEnabled: &other}) {
		t.Fatal("bool pointer identity broke retries")
	}
}

func TestAttentionUnreadCompletionOutlivesPushTTL(t *testing.T) {
	now := time.Now()
	o := attentionFixture()
	o.Lead.LatestTurnStatus = "inProgress"
	o.Lead.CurrentState = "working"
	b, _ := reduceAttention(attentionBaseline{}, o, now)
	o.Lead.LatestTurnStatus = "completed"
	o.Lead.CurrentState = "idle"
	b, emit := reduceAttention(b, o, now.Add(time.Second))
	if !emit {
		t.Fatal("completion missing")
	}
	id, revision, expires := b.Event.ID, b.Revision, b.Event.ExpiresAt
	for elapsed := 30 * time.Second; elapsed <= 6*time.Minute; elapsed += 30 * time.Second {
		b, emit = reduceAttention(b, o, now.Add(elapsed))
		if emit || b.Event == nil || b.Event.ID != id || b.Revision != revision || b.Event.ExpiresAt != expires {
			t.Fatal("title attention expired or was re-alerted by delivery TTL")
		}
	}
}

func TestAttentionRestartGapPreservesKnownEventsWithoutDiscoveringOldCompletion(t *testing.T) {
	start := time.Now()
	for _, kind := range []string{"done", "input"} {
		t.Run(kind, func(t *testing.T) {
			o := attentionFixture()
			o.Lead.LatestTurnStatus = "inProgress"
			o.Lead.CurrentState = "working"
			b, _ := reduceAttention(attentionBaseline{}, o, start)
			if kind == "done" {
				o.Lead.LatestTurnStatus = "completed"
				o.Lead.CurrentState = "idle"
			} else {
				o.Lead.CurrentState = "waiting"
				o.Requests = []attentionRequest{{"native-request", "gpt6reviewer0", "input"}}
			}
			b, emit := reduceAttention(b, o, start.Add(time.Second))
			if !emit {
				t.Fatal("event not emitted")
			}
			known := *b.Event
			// Persist/reload before each isolated gap; no continuous ticks preserve it.
			encoded, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			for _, gap := range []time.Duration{2 * time.Minute, 6 * time.Minute, 16 * time.Minute} {
				var restored attentionBaseline
				if err := json.Unmarshal(encoded, &restored); err != nil {
					t.Fatal(err)
				}
				after, replayed := reduceAttention(restored, o, start.Add(gap))
				if replayed || after.Event == nil || *after.Event != known || after.Revision != b.Revision {
					t.Fatalf("gap %s lost/replayed persisted attention", gap)
				}
			}
		})
	}
	o := attentionFixture()
	o.Lead.LatestTurnStatus = "inProgress"
	o.Lead.CurrentState = "working"
	b, _ := reduceAttention(attentionBaseline{}, o, start)
	o.Lead.LatestTurnStatus = "completed"
	o.Lead.CurrentState = "idle"
	for _, turn := range []string{o.Lead.CurrentTurnID, "new-during-downtime"} {
		o.Lead.CurrentTurnID = turn
		after, emit := reduceAttention(b, o, start.Add(16*time.Minute))
		if emit || after.Event != nil {
			t.Fatal("first-discovered completion after downtime manufactured attention")
		}
	}
}

func TestAttentionManagedReceiptPreferenceIsOptionalStrictBoolean(t *testing.T) {
	request := map[string]json.RawMessage{"kind": json.RawMessage(`"new"`), "slug": json.RawMessage(`"example"`), "goal": json.RawMessage(`"Work"`),
		"team": json.RawMessage(`"solo"`), "catalogDigest": json.RawMessage(`"` + strings.Repeat("a", 64) + `"`)}
	if err := validateManagedCreationRequestShape(request); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"true", "false"} {
		request["browserNotificationsEnabled"] = json.RawMessage(raw)
		if err := validateManagedCreationRequestShape(request); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"null", `"false"`, "0", "[]", "{}"} {
		request["browserNotificationsEnabled"] = json.RawMessage(raw)
		if err := validateManagedCreationRequestShape(request); err == nil {
			t.Fatal("malformed preference accepted", raw)
		}
	}
}
