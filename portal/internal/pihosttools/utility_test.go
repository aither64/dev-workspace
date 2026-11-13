package pihosttools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type utilityHandlersFake struct {
	calls  int
	caller PiUtilityCaller
}

func (f *utilityHandlersFake) MonitorStatus(_ context.Context, c PiUtilityCaller) (any, error) {
	f.calls++
	f.caller = c
	return "held", nil
}
func (f *utilityHandlersFake) MonitorReport(_ context.Context, c PiUtilityCaller, r monitoroperation.ReportRequest) (string, error) {
	f.calls++
	f.caller = c
	return "durable-event", nil
}
func TestPiUtilityToolsHaveNoRetainedMemberIdentityOrActions(t *testing.T) {
	c := PiUtilityCaller{Utility: monitoroperation.PiUtilityRecord{Session: monitoroperation.PiSessionRecord{Workspace: "/workspace", Slug: "2026-11-12-fixture", Incarnation: strings.Repeat("a", 64)}, OperationID: strings.Repeat("b", 32), Incarnation: strings.Repeat("c", 64), NativeID: "utility-native"}, Generation: strings.Repeat("d", 64), InputOperationID: strings.Repeat("e", 32)}
	fake := &utilityHandlersFake{}
	tools, err := BindUtilityTools(c, fake)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"report_to_lead", "team_message", "request_user_input_async", "session_communication"} {
		if _, err := tools.Call(context.Background(), name, json.RawMessage(`{}`)); !errors.Is(err, piruntime.ErrScope) {
			t.Fatal(name, err)
		}
	}
	for _, raw := range []string{`{"action":"start"}`, `{"action":"status","from":"lead"}`, `{"action":"status","operationId":"other"}`, `{"action":"status","action":"report"}`} {
		if _, err := tools.Call(context.Background(), "monitor_operation", json.RawMessage(raw)); err == nil {
			t.Fatal("forged utility action accepted")
		}
	}
	if fake.calls != 0 {
		t.Fatal("refusal invoked utility domain")
	}
	if len(tools.HostTools()) != 0 {
		t.Fatal("utility handler advertised unsupported live tools")
	}
	if _, err := tools.Call(context.Background(), "monitor_operation", json.RawMessage(`{"action":"status"}`)); err != nil || fake.calls != 1 || fake.caller != c {
		t.Fatal("lost bound utility subject", err)
	}
	tools, _ = BindUtilityTools(c, nil)
	if _, err := tools.Call(context.Background(), "monitor_operation", json.RawMessage(`{"action":"status"}`)); !errors.Is(err, piruntime.ErrUnavailable) {
		t.Fatal(err)
	}
	tools, _ = BindUtilityTools(c, MonitorDomainHandlers{})
	if _, err := tools.Call(context.Background(), "monitor_operation", json.RawMessage(`{"action":"status"}`)); !errors.Is(err, piruntime.ErrUnavailable) {
		t.Fatal("domain handler fabricated missing authority", err)
	}
}
