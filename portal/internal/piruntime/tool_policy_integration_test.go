package piruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestNativeToolPolicyCreateApplyInspectReloadAndReplay(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	plan := ToolPolicyTarget{Mode: "plan", Revision: 1, Active: []string{"read"}, RegisteredCeiling: []string{"read", "write"}}
	plan.Digest = ToolPolicyDigest(plan)
	request := CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: settingsFixture(), ToolPolicy: &plan}
	state, err := m.Create(ctx, runtime.Runtime, request)
	if err != nil || state.Persisted {
		t.Fatal(state, err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	proof, err := m.InspectToolPolicy(ctx, handle)
	if err != nil || ValidateToolPolicyProof(proof, plan, handle.Generation) != nil {
		t.Fatal(proof, err)
	}
	c, _ := h.s.member(handle.Conversation)
	if c.record.Schema != 2 || c.record.PendingToolPolicy != nil || c.record.ToolPolicy == nil {
		t.Fatal(c.record)
	}
	if _, err := m.Create(ctx, runtime.Runtime, request); err != nil {
		t.Fatal("exact initial policy replay", err)
	}
	missing := request
	missing.ToolPolicy = nil
	if _, err := m.Create(ctx, runtime.Runtime, missing); !errors.Is(err, ErrConflict) {
		t.Fatal("nil policy replay accepted", err)
	}
	changed := plan
	changed.Active = []string{}
	changed.Digest = ToolPolicyDigest(changed)
	different := request
	different.ToolPolicy = &changed
	if _, err := m.Create(ctx, runtime.Runtime, different); !errors.Is(err, ErrConflict) {
		t.Fatal("changed initial policy replay", err)
	}
	path := filepath.Join(start.Session.Workspace, "work", start.Session.Slug, "owned-policy-result")
	send := func(id string) {
		t.Helper()
		result, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "one explicit policy input", ClientUserMessageID: id})
		if err != nil || result.Outcome != backend.Accepted {
			t.Fatal(result, err)
		}
		waitIdle(t, m, ctx, handle)
	}
	send(strings.Repeat("1", 32))
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Plan native write took effect", err)
	}
	def := plan
	def.Mode = "default"
	def.Revision = 2
	def.Active = []string{"read", "write"}
	def.Digest = ToolPolicyDigest(def)
	if _, err := m.ApplyToolPolicy(ctx, handle, 2, def); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong revision mutated policy", err)
	}
	narrow := def
	narrow.Active = []string{"read"}
	narrow.RegisteredCeiling = []string{"read"}
	narrow.Digest = ToolPolicyDigest(narrow)
	if _, err := m.ApplyToolPolicy(ctx, handle, 1, narrow); !errors.Is(err, ErrConflict) {
		t.Fatal("ceiling replacement accepted", err)
	}
	result, err := m.ApplyToolPolicy(ctx, handle, 1, def)
	if err != nil || result.Outcome != "applied" || ValidateToolPolicyProof(result.Proof, def, handle.Generation) != nil {
		t.Fatal(result, err)
	}
	if _, err := m.Create(ctx, runtime.Runtime, request); !errors.Is(err, ErrUnavailable) {
		t.Fatal("advanced policy Create replay", err)
	}
	send(strings.Repeat("2", 32))
	if data, err := os.ReadFile(path); err != nil || string(data) != "Default permits" {
		t.Fatal("Default approved native write", string(data), err)
	}
	back := plan
	back.Revision = 3
	if result, err := m.ApplyToolPolicy(ctx, handle, 2, back); err != nil || result.Outcome != "applied" {
		t.Fatal(result, err)
	}
	send(strings.Repeat("3", 32))
	if data, err := os.ReadFile(path); err != nil || string(data) != "Default permits" {
		t.Fatal("returned Plan overwrote native file", string(data), err)
	}
	if _, err := m.Create(ctx, runtime.Runtime, request); !errors.Is(err, ErrUnavailable) {
		t.Fatal("same-mode later revision replayed initial Create", err)
	}
	if stop, err := m.Unload(ctx, handle); err != nil || !stop.VerifiedExit {
		t.Fatal(stop, err)
	}
	if _, err := m.InspectToolPolicy(ctx, handle); !errors.Is(err, ErrUnavailable) {
		t.Fatal("Inspect cold policy loaded worker", err)
	}
	if _, err := m.Load(ctx, handle); err != nil {
		t.Fatal(err)
	}
	proof, err = m.InspectToolPolicy(ctx, handle)
	if err != nil || ValidateToolPolicyProof(proof, back, handle.Generation) != nil {
		t.Fatal("retained policy reload", proof, err)
	}
	if stop, err := m.Unload(ctx, handle); err != nil || !stop.VerifiedExit {
		t.Fatal(stop, err)
	}
}

func TestNativeToolPolicyInitialPendingNeverRestartsCreate(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	policy := ToolPolicyTarget{Mode: "plan", Revision: 1, Active: []string{}, RegisteredCeiling: []string{}}
	policy.Digest = ToolPolicyDigest(policy)
	request := CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: settingsFixture(), ToolPolicy: &policy}
	setups := 0
	h.s.start = func(context.Context, workerMetadata, agentbackend.ExecutionHandle, workerConfig, func()) (worker, error) {
		setups++
		return nil, ErrUnavailable
	}
	if _, err := m.Create(ctx, runtime.Runtime, request); err == nil {
		t.Fatal("missing initialization failure")
	}
	if _, err := m.Create(ctx, runtime.Runtime, request); !errors.Is(err, ErrHeld) {
		t.Fatal("pending initialization replayed", err)
	}
	if setups != 1 {
		t.Fatal("repeated native initialization", setups)
	}
}
