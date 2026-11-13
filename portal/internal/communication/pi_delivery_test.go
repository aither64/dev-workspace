package communication

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func TestPiCommunicationRequiresExactGrantCurrentLeadAndRecipientPolicy(t *testing.T) {
	session := agentbackend.SessionRef{Workspace: "/workspace", Slug: "2026-11-12-from", Incarnation: strings.Repeat("a", 64)}
	from := agentbackend.ExecutionHandle{Conversation: agentbackend.ConversationRef{Session: session, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "from-native"}, Generation: strings.Repeat("c", 64)}
	caller := teamruntime.PiCaller{Handle: from, Runtime: piruntime.RuntimeRef{Session: session, Generation: from.Generation, InstanceID: "11111111-1111-4111-8111-111111111111", RequestID: "22222222-2222-4222-8222-222222222222", Digest: strings.Repeat("d", 64)}}
	target := from
	target.Conversation.Session.Slug = "2026-11-12-to"
	target.Conversation.NativeID = "to-native"
	grant := PiGrantProjection{ID: strings.Repeat("e", 32), A: from.Conversation, B: target.Conversation, TypedEndpointsSupported: true}
	for _, tc := range []struct {
		name   string
		mutate func(*teamruntime.PiCaller, *PiGrantProjection)
		want   error
	}{
		{"revoked", func(c *teamruntime.PiCaller, g *PiGrantProjection) { g.Revoked = true }, teamruntime.ErrPiPolicy},
		{"held", func(c *teamruntime.PiCaller, g *PiGrantProjection) { g.RecipientHeld = true }, teamruntime.ErrPiHeld},
		{"no codec", func(c *teamruntime.PiCaller, g *PiGrantProjection) { g.TypedEndpointsSupported = false }, teamruntime.ErrPiUnavailable},
		{"wrong pair", func(c *teamruntime.PiCaller, g *PiGrantProjection) { g.B.NativeID = "another-native" }, teamruntime.ErrPiPolicy},
		{"member", func(c *teamruntime.PiCaller, g *PiGrantProjection) { c.Handle.Conversation.Member = "implementer0" }, teamruntime.ErrPiPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, g := caller, grant
			tc.mutate(&c, &g)
			calls := 0
			err := WithPiDeliveryPolicy(context.Background(), c, target, g, func() error { calls++; return nil })
			if !errors.Is(err, tc.want) || calls != 0 {
				t.Fatalf("%v effects=%d", err, calls)
			}
		})
	}
	calls := 0
	if err := WithPiDeliveryPolicy(context.Background(), caller, target, grant, func() error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatalf("%v effects=%d", err, calls)
	}
}
