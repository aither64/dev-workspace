package piruntime

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestResourceOriginalIDAndExactProcessStop(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	status, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	group, err := processCgroup(h.observation.MainPID)
	if err != nil {
		t.Fatal(err)
	}
	h.s.handshake.ControlGroup = group
	request := ResourceRequest{ResourceID: "22222222-2222-4222-8222-222222222222", OperationID: "33333333-3333-4333-8333-333333333333", Cwd: start.Session.Workspace, Argv: []string{h.metadata.Node, "-e", "setTimeout(()=>{},1000)"}}
	request.Digest = ResourceDigest(status.Runtime, request)
	first, err := h.s.startResource(ctx, request)
	if err != nil || first.Phase != "running" || first.Handle.PID <= 0 {
		t.Fatal(first, err)
	}
	again, err := h.s.startResource(ctx, request)
	if err != nil || again.Handle != first.Handle {
		t.Fatal("resource start replayed", again, err)
	}
	changed := request
	changed.Argv = []string{h.metadata.Node, "-e", "process.exit(0)"}
	changed.Digest = ResourceDigest(status.Runtime, changed)
	if _, err := h.s.startResource(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	wrong := first.Handle
	wrong.PIDStart = "wrong"
	if _, err := h.s.stopResource(ctx, wrong); !errors.Is(err, ErrScope) {
		t.Fatal("changed process identity accepted", err)
	}
	stop, err := h.s.stopResource(ctx, first.Handle)
	if err != nil || !stop.VerifiedExit {
		t.Fatal(stop, err)
	}
	for {
		observed, err := h.s.inspectResource(first.Handle)
		if err != nil {
			t.Fatal(err)
		}
		if observed.Phase == "stopped" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	// A lost native-start observation has no PID proof and never restarts.
	request.ResourceID = "44444444-4444-4444-8444-444444444444"
	request.OperationID = "55555555-5555-4555-8555-555555555555"
	request.Digest = ResourceDigest(status.Runtime, request)
	path, _ := h.s.resourcePath(request.ResourceID)
	record := resourceRecord{Schema: 1, Request: request, Status: ResourceStatus{Handle: ResourceHandle{Runtime: status.Runtime, ResourceID: request.ResourceID, OperationID: request.OperationID}, Phase: "starting"}}
	if err := writePrivate(path, record); err != nil {
		t.Fatal(err)
	}
	held, err := h.s.startResource(ctx, request)
	if err != nil || held.Phase != "unknown" || held.Handle.PID != 0 {
		t.Fatal(held, err)
	}
	if !strings.Contains(h.s.handshake.ControlGroup, "/") {
		t.Fatal("fixture cgroup unavailable")
	}
}
