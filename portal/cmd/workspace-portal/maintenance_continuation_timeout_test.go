package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/coder/websocket"
)

func TestMaintenanceCancellationContinuationTimeoutRetriesOriginalIdentity(t *testing.T) {
	var blocked, reached atomic.Bool
	blocked.Store(true)
	m, o, events := maintenancePauseFixture(t, true, func(_ *websocket.Conn, request map[string]any) bool {
		if request["method"] == "thread/resume" && blocked.Load() {
			reached.Store(true)
			return false
		}
		return true
	})
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := m.pause(parent, o.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.Update(parent, o.OperationID, 0, func(saved *maintenance.Operation) error { saved.CancelRequested = true; return nil }); err != nil {
		t.Fatal(err)
	}
	m.continuationTimeout = 300 * time.Millisecond
	result, err := m.continueParticipants(parent, o.OperationID)
	if !reached.Load() || !errors.Is(err, context.DeadlineExceeded) || parent.Err() != nil || result.Status == "continued" {
		t.Fatal("unanswered native continuation was not bounded", result, err, parent.Err())
	}
	saved, err := m.store.Load(o.OperationID)
	if err != nil || saved.ContinuationsSettled || saved.Participants[0].ContinuationID != o.Participants[0].ContinuationID || saved.Participants[0].ContinuationDisposition != "submitting" {
		t.Fatal("timeout lost continuation identity or uncertainty", saved, err)
	}
	blocked.Store(false)
	m.continuationTimeout = 3 * time.Second
	for range 2 {
		result, err = m.continueParticipants(parent, o.OperationID)
		if err != nil || result.Status != "continued" {
			t.Fatal("original cancellation could not resume", result, err)
		}
	}
	if strings.Count(fmt.Sprint(*events), "send:thread") != 1 {
		t.Fatal("retry duplicated resume", *events)
	}
	saved, err = m.store.Load(o.OperationID)
	if err != nil || !saved.ContinuationsSettled || saved.Participants[0].ContinuationID != o.Participants[0].ContinuationID {
		t.Fatal("successful retry replaced original continuation", saved, err)
	}
}
