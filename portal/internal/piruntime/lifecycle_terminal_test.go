package piruntime

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestVerifyLifecycleTerminalPreservesOriginalRecords(t *testing.T) {
	m, base, start, ctx := supervisorFixture(t)
	host := newUpgradeFixtureHost(base)
	m.config.Host = host
	first, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	attachFixture(t, m, base.s, ctx, first.Runtime, &workflowFixtureHandler{invalidated: func(WorkflowInvalidation) {}})
	awaitWorkflowAttachment(t, base.s, ctx)
	policy := policyTarget("default", 1, []string{}, []string{})
	member, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: settingsFixture(), ToolPolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Unload(ctx, agentbackend.ExecutionHandle{Conversation: member.Conversation, Generation: first.Runtime.Generation}); err != nil {
		t.Fatal(err)
	}
	saved, err := host.Stop(ctx, first.Runtime, first.Observation)
	if err != nil {
		t.Fatal(err)
	}
	directory, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	path := filepath.Join(directory, "members", "lead", member.Conversation.Incarnation, "conversation.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runtimeBytes, err := os.ReadFile(filepath.Join(directory, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	starts, stops := host.starts, host.stops
	drainFailed, drains := false, 0
	m.config.verifyDrain = func(HostObservation) error {
		drains++
		if drainFailed {
			return ErrHeld
		}
		return nil
	}
	if err := m.VerifyLifecycleTerminal(ctx, first.Runtime, saved); err != nil || drains != 1 {
		t.Fatal("exact settled terminal refused", err, drains)
	}
	drainFailed = true
	if err := m.VerifyLifecycleTerminal(ctx, first.Runtime, saved); err == nil {
		t.Fatal("failed current drain admitted")
	}
	drainFailed = false
	wrong := saved
	wrong.Generation = strings.Repeat("d", 64)
	if err := m.VerifyLifecycleTerminal(ctx, first.Runtime, wrong); err == nil {
		t.Fatal("wrong saved tuple admitted")
	}
	wrongRef := first.Runtime
	wrongRef.RequestID = "22222222-2222-4222-8222-222222222222"
	if err := m.VerifyLifecycleTerminal(ctx, wrongRef, saved); err == nil {
		t.Fatal("wrong current tuple admitted")
	}
	for _, held := range []string{"loaded", "unknown"} {
		var record conversationRecord
		if err := readPrivate(path, &record); err != nil {
			t.Fatal(err)
		}
		if held == "loaded" {
			record.Loaded = true
		} else {
			record.HeldReason = "unknown original outcome"
		}
		if err := writePrivate(path, record); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(path)
		if err := m.VerifyLifecycleTerminal(ctx, first.Runtime, saved); err == nil {
			t.Fatal("retained owner admitted", held)
		}
		after, _ := os.ReadFile(path)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("verification changed held audit", held)
		}
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.VerifyLifecycleTerminal(context.Background(), first.Runtime, saved); err != nil {
		t.Fatal("unchanged terminal replay refused", err)
	}
	after, _ := os.ReadFile(filepath.Join(directory, "runtime.json"))
	if !reflect.DeepEqual(runtimeBytes, after) || starts != host.starts || stops != host.stops {
		t.Fatal("verification mutated runtime or host")
	}
}
