package piruntime

import (
	"context"
	"encoding/json"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	conversationhttp "github.com/aither64/codex-web/conversation"
	"github.com/aither64/codex-web/conversation/backend"
)

func TestReadLocalHTTPCallCountAndActivity(t *testing.T) {
	m, _, w, handle, _, ctx, _ := adapterHTTPFixture(t)
	start, err := m.ReadStart(ctx, handle.Conversation.Session)
	if err != nil {
		t.Fatal(err)
	}
	transcriptContractFixture(t, m, start, `{"piTranscriptSnapshot":1,"piInputDelivery":1}`)
	m.config.activityContract = func(app string) (bool, error) {
		if app != start.AppStorePath {
			t.Fatal("selected portal rather than frozen worker", app)
		}
		return true, nil
	}
	w.snapshot.Entries = []json.RawMessage{nativeRow("entry", "user", "original", "", "")}
	handler, err := conversationhttp.NewBackendHandler(conversationhttp.BackendOptions{AllowedOrigins: []string{"https://portal.test"}, Logger: log.New(io.Discard, "", 0), Resolver: conversationhttp.BackendResolverFunc(func(ctx context.Context, _ conversationhttp.ResolveRequest) (conversationhttp.BackendTarget, error) {
		ops, delivery, err := m.BindRead(ctx, handle)
		return conversationhttp.BackendTarget{Identity: BackendIdentity(handle), Operations: ops, Capabilities: conversationhttp.Capabilities{Read: true, InputDelivery: delivery}}, err
	})})
	if err != nil {
		t.Fatal(err)
	}
	snapshots, capabilities := w.snapshots, w.inputCapabilities
	page := adapterHTTPRead(t, handler, "thread/page", http.StatusOK, "")
	if len(page.Entries) != 1 || !page.InputDelivery || w.snapshots-snapshots != 1 || w.inputCapabilities-capabilities != 1 {
		t.Fatalf("latest HTTP read: entries=%d snapshot=%d capability=%d", len(page.Entries), w.snapshots-snapshots, w.inputCapabilities-capabilities)
	}
	snapshots = w.snapshots
	request := httptest.NewRequest("GET", "/codex/conversations/trusted/activity", nil)
	request.Header.Set("Origin", "https://portal.test")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var activity backend.ActivitySnapshot
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &activity) != nil || activity.CurrentState != "working" || activity.WorkingMS != 12 || w.activities != 1 || w.snapshots != snapshots {
		t.Fatalf("activity HTTP: %d %s snapshots=%d", response.Code, response.Body.String(), w.snapshots-snapshots)
	}
	ops, _, err := m.BindRead(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ops.Read.Read(ctx, BackendIdentity(handle))
	if err != nil {
		t.Fatal(err)
	}
	first.Entries[0].Text = "presentation mutation"
	second, err := ops.Read.Read(ctx, BackendIdentity(handle))
	if err != nil || second.Entries[0].Text != "original" {
		t.Fatal("read result leaked mutation", second, err)
	}
	directory, _ := stateDirectory(m.config.UserStateRoot, handle.Conversation.Session)
	var originalBytes json.RawMessage
	if err := readPrivate(filepath.Join(directory, "runtime.json"), &originalBytes); err != nil {
		t.Fatal(err)
	}
	records, err := m.records(handle.Conversation.Session)
	if err != nil {
		t.Fatal(err)
	}
	original := records.Requests[records.CurrentRequestID]
	changed := original
	changed.Runtime.Generation = strings.Repeat("f", 64)
	records.Requests[records.CurrentRequestID] = changed
	if err := writePrivate(filepath.Join(directory, "runtime.json"), records); err != nil {
		t.Fatal(err)
	}
	_, stale := ops.Read.Read(ctx, BackendIdentity(handle))
	if err := writePrivate(filepath.Join(directory, "runtime.json"), originalBytes); err != nil {
		t.Fatal(err)
	}
	if stale == nil {
		t.Fatal("changed current generation reused request result")
	}
	m.config.activityContract = func(string) (bool, error) { return false, nil }
	legacy, err := m.Bind(ctx, handle)
	if err != nil || legacy.Activity != nil {
		t.Fatal("legacy worker advertised activity", err)
	}
	if w.sends != 0 || w.configures != 0 {
		t.Fatal("read mutated worker")
	}
	t.Log("latest admitted backend read: one native snapshot + one capability; Activity: no snapshot")
}

func TestPinnedSDKActivityThroughCurrentWorker(t *testing.T) {
	m, host, start, ctx := supervisorFixture(t)
	host.metadata.Protocol = 2
	m.config.activityContract = func(app string) (bool, error) {
		if app != start.AppStorePath {
			t.Fatal("activity contract used wrong package", app)
		}
		return true, nil
	}
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: "22222222-2222-4222-8222-222222222222", Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	ops, err := m.Bind(ctx, handle)
	if err != nil || ops.Activity == nil {
		t.Fatal("activity bind", err)
	}
	initial, err := ops.Activity.ReadActivity(ctx, BackendIdentity(handle))
	if err != nil || initial.CurrentState != "idle" {
		t.Fatal(initial, err)
	}
	result, err := ops.Send.Send(ctx, BackendIdentity(handle), backend.SendRequest{Message: "activity fixture", ClientUserMessageID: "77777777-7777-4777-8777-777777777777"})
	if err != nil || result.Outcome != backend.Accepted {
		t.Fatal(result, err)
	}
	waitIdle(t, m, ctx, handle)
	activity, err := ops.Activity.ReadActivity(ctx, BackendIdentity(handle))
	if err != nil || activity.ThreadID != handle.Conversation.NativeID || activity.CurrentState != "idle" || activity.ReceivedMessages != 1 || activity.SentMessages < 1 || activity.CoverageComplete || activity.StartedAtMS == 0 || activity.CompletedAtMS == 0 {
		t.Fatal(activity, err)
	}
	t.Logf("actual current shared SDK activity: state=%s sent=%d received=%d workingMs=%d partial=%t", activity.CurrentState, activity.SentMessages, activity.ReceivedMessages, activity.WorkingMS, !activity.CoverageComplete)
}

func TestActivityV1CanonicalMetrics(t *testing.T) {
	m, _, w, h, _, ctx, _ := adapterHTTPFixture(t)
	m.config.activityContract = func(string) (bool, error) { return true, nil }
	ops, err := m.Bind(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	raw := backend.ActivitySnapshot{ThreadID: h.Conversation.NativeID, CurrentState: "waiting",
		SentMessages: 2, ReceivedMessages: 3, TotalToolCalls: 7, CountsComplete: true,
		WorkingMS: 20000, WaitingMS: 10000, OpenWaitingMS: 6000, IdleMS: 120000,
		ObservedAtMS: 1700000000000, StateSinceMS: 1699999994000, CoverageReason: "worker attachment"}
	w.activity = &raw
	got, err := ops.Activity.ReadActivity(ctx, BackendIdentity(h))
	if err != nil || got.SentMessages != 3 || got.ReceivedMessages != 2 || got.TotalToolCalls != 7 ||
		got.WaitingMS != 4000 || got.OpenWaitingMS != 6000 || got.WaitingMS+got.OpenWaitingMS != raw.WaitingMS ||
		got.ObservedAtMS != raw.ObservedAtMS || got.StateSinceMS != raw.StateSinceMS || !got.CountsComplete || got.CoverageComplete || got.CoverageReason != raw.CoverageReason {
		t.Fatal("native-v1 mapping", got, err)
	}
	closed := raw
	closed.CurrentState, closed.OpenWaitingMS = "idle", 0
	w.activity = &closed
	if got, err = ops.Activity.ReadActivity(ctx, BackendIdentity(h)); err != nil || got.WaitingMS != 10000 || got.IdleMS != 120000 {
		t.Fatal("closed waiting changed", got, err)
	}
	for _, change := range []func(*backend.ActivitySnapshot){
		func(v *backend.ActivitySnapshot) { v.SentMessages = -1 },
		func(v *backend.ActivitySnapshot) { v.ReceivedMessages = -1 },
		func(v *backend.ActivitySnapshot) { v.TotalToolCalls = -1 },
		func(v *backend.ActivitySnapshot) { v.WorkingMS = -1 },
		func(v *backend.ActivitySnapshot) { v.OpenWaitingMS = -1 },
		func(v *backend.ActivitySnapshot) { v.OpenWaitingMS = v.WaitingMS + 1 },
		func(v *backend.ActivitySnapshot) { v.CurrentState = "idle" },
	} {
		invalid := raw
		change(&invalid)
		w.activity = &invalid
		if _, err := ops.Activity.ReadActivity(ctx, BackendIdentity(h)); err == nil {
			t.Fatal("contradictory native activity accepted", invalid)
		}
	}
	if w.sends != 0 || w.configures != 0 {
		t.Fatal("activity read mutated native state")
	}
}
