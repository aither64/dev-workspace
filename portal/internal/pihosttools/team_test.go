package pihosttools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type teamHandlersFake struct {
	calls   int
	caller  teamruntime.PiCaller
	message teamruntime.PiMessage
}

func (f *teamHandlersFake) Report(_ context.Context, c teamruntime.PiCaller, id, text string) (teamruntime.PiDeliveryResult, error) {
	f.calls++
	f.caller = c
	f.message = teamruntime.PiMessage{ID: id, To: "lead", Text: text}
	return teamruntime.PiDeliveryResult{Disposition: teamruntime.PiHeld}, nil
}
func (f *teamHandlersFake) Peer(ctx context.Context, c teamruntime.PiCaller, m teamruntime.PiMessage) (teamruntime.PiDeliveryResult, error) {
	f.calls++
	f.caller = c
	f.message = m
	return teamruntime.PiDeliveryResult{Disposition: teamruntime.PiUnavailable}, teamruntime.ErrPiUnavailable
}

func TestPiToolsCaptureCallerAndRejectAllModelAuthority(t *testing.T) {
	s := agentbackend.SessionRef{Workspace: "/workspace", Slug: "2026-11-12-fixture", Incarnation: strings.Repeat("a", 64)}
	h := agentbackend.ExecutionHandle{Conversation: agentbackend.ConversationRef{Session: s, Backend: agentbackend.Pi, Member: "reviewer0", Incarnation: strings.Repeat("b", 64), NativeID: "native-reviewer"}, Generation: strings.Repeat("c", 64)}
	c := teamruntime.PiCaller{Handle: h, Runtime: piruntime.RuntimeRef{Session: s, Generation: h.Generation, InstanceID: "11111111-1111-4111-8111-111111111111", RequestID: "22222222-2222-4222-8222-222222222222", Digest: strings.Repeat("d", 64)}}
	fake := &teamHandlersFake{}
	tools, err := BindTeamTools(c, fake)
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"message_id":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","message":"Result"}`
	for _, extra := range []string{`"from":"lead"`, `"workspace":"/other"`, `"generation":"new"`, `"native_id":"other"`} {
		raw := strings.TrimSuffix(valid, "}") + "," + extra + "}"
		if _, err := tools.Call(context.Background(), "report_to_lead", json.RawMessage(raw)); !errors.Is(err, teamruntime.ErrPiPolicy) {
			t.Fatalf("accepted authority argument: %s %v", extra, err)
		}
	}
	for _, raw := range []string{valid + `{}`, `{"message_id":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","message":"one","message":"two"}`, `{"message_id":null,"message":"one"}`} {
		if _, err := tools.Call(context.Background(), "report_to_lead", json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted malformed args: %s", raw)
		}
	}
	if fake.calls != 0 {
		t.Fatal("factory/refusal forwarded a result")
	}
	if len(tools.HostTools()) != 0 {
		t.Fatal("inert handler advertised live callback support")
	}
	if _, err := tools.Call(context.Background(), "report_to_lead", json.RawMessage(valid)); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 || fake.caller != c || fake.message.To != "lead" {
		t.Fatalf("wrong binding: %+v", fake)
	}
	tools, _ = BindTeamTools(c, nil)
	if _, err := tools.Call(context.Background(), "report_to_lead", json.RawMessage(valid)); !errors.Is(err, teamruntime.ErrPiUnavailable) {
		t.Fatal(err)
	}
}
