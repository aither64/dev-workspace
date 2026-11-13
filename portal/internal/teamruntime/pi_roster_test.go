package teamruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type piWriteFake struct {
	session    agentbackend.SessionRef
	generation string
	calls      int
}

func (f *piWriteFake) WithPiTeamWrite(ctx context.Context, s agentbackend.SessionRef, g string, run func() error) error {
	if s != f.session || g != f.generation {
		return piruntime.ErrScope
	}
	f.calls++
	return run()
}
func piRosterFixture(workspace string) PiRoster {
	now := time.Now().UTC()
	return PiRoster{Schema: 2, Workspace: workspace, Slug: "2026-11-12-fixture", Revision: 1, CreatedAt: now, UpdatedAt: now, Backend: "pi", BackendContract: 1, SessionIncarnation: strings.Repeat("a", 64), LeadIncarnation: strings.Repeat("b", 64), Members: []PiRosterMember{
		{Address: "implementer0", Role: "implementer", Index: 0, Model: "retained-model", Effort: "xhigh", Behavior: "implementer", Purpose: "implementation", Instructions: "Retain instructions.", Access: "workspace_write", State: "creating", AddedAt: now, Incarnation: strings.Repeat("c", 64), CreationID: strings.Repeat("d", 64)},
	}}
}
func TestPiRosterStrictCodecAndLegacyRoundtrip(t *testing.T) {
	r := piRosterFixture(t.TempDir())
	data, _ := json.Marshal(r)
	decoded, err := DecodePiRoster(data)
	if err != nil {
		t.Fatal(err)
	}
	round, _ := json.Marshal(decoded)
	if string(data) != string(round) {
		t.Fatal("Pi roundtrip changed values")
	}
	for _, bad := range []string{
		strings.Replace(string(data), `"index":0,`, "", 1),
		strings.Replace(string(data), `"schema":2`, `"schema":2,"schema":2`, 1),
		strings.Replace(string(data), `"backend":"pi"`, `"backend":"pi","rootThreadId":"fake-codex"`, 1),
		strings.Replace(string(data), `"sessionIncarnation":"`+strings.Repeat("a", 64)+`"`, `"sessionIncarnation":null`, 1),
		string(data) + `{}`,
	} {
		if _, err := DecodePiRoster([]byte(bad)); err == nil {
			t.Fatalf("accepted ambiguous codec: %s", bad)
		}
	}
	legacy := Roster{Schema: 1, Workspace: r.Workspace, Slug: r.Slug, RootThreadID: "old-root", Revision: 1, Members: []Member{}}
	old, _ := json.MarshalIndent(legacy, "", "  ")
	record, err := DecodeRosterRecord(old)
	if err != nil {
		t.Fatal(err)
	}
	same, err := record.MarshalJSON()
	if err != nil || string(same) != string(old) {
		t.Fatal("legacy bytes changed", err)
	}
	var reader Roster
	if err := json.Unmarshal(data, &reader); err != nil {
		t.Fatal(err)
	}
	if reader.Validate(r.Workspace, r.Slug, "old-root") == nil {
		t.Fatal("old reader accepted schema2")
	}
}
func TestPiRosterPreservesCreationReceiptAndDistinctInitialInputIDs(t *testing.T) {
	r := piRosterFixture(t.TempDir())
	creation := r.Members[0].CreationID
	for _, input := range []string{strings.Repeat("e", 32), "12345678-1234-4234-8234-123456789abc"} {
		r.Members[0].InitialInputID = input
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodePiRoster(data)
		if err != nil || decoded.Members[0].CreationID != creation || decoded.Members[0].InitialInputID != input {
			t.Fatal("creation/input reference changed or original receipt refused", err)
		}
	}
	for _, invalid := range []string{strings.Repeat("d", 32), "12345678-1234-4234-8234-123456789abc", strings.Repeat("D", 64), strings.Repeat("d", 63), strings.Repeat("d", 65), creation + " "} {
		r.Members[0].CreationID = invalid
		data, _ := json.Marshal(r)
		if _, err := DecodePiRoster(data); err == nil {
			t.Fatal("nonreceipt creation ID accepted", invalid)
		}
	}
	r.Members[0].CreationID = creation
	r.Members[0].InitialInputID = strings.Repeat("e", 64)
	data, _ := json.Marshal(r)
	if _, err := DecodePiRoster(data); err == nil {
		t.Fatal("creation reference format widened initial input IDs")
	}
}

func TestPiRosterWriterRequiresCurrentAdmissionBeforeAnyFile(t *testing.T) {
	root := t.TempDir()
	workspace := t.TempDir()
	store, err := NewStore(root, workspace)
	if err != nil {
		t.Fatal(err)
	}
	r := piRosterFixture(workspace)
	generation := strings.Repeat("e", 64)
	if err := store.SavePi(context.Background(), nil, generation, 0, r); !errors.Is(err, piruntime.ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing admission created state")
	}
	gate := &piWriteFake{session: r.Scope().Ref(), generation: generation}
	if err := store.SavePi(context.Background(), gate, strings.Repeat("f", 64), 0, r); !errors.Is(err, piruntime.ErrScope) {
		t.Fatal(err)
	}
	if err := store.SavePi(context.Background(), gate, generation, 0, r); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadPi(r.Slug)
	if err != nil || loaded.Members[0].CreationID != r.Members[0].CreationID {
		t.Fatal(err)
	}
	if _, err := store.Load(r.Slug, "legacy-root"); err == nil {
		t.Fatal("legacy writer can read Pi roster")
	}
	next := r
	next.Revision = 2
	next.Members = append([]PiRosterMember(nil), r.Members...)
	next.Members[0].InitialInputID = strings.Repeat("f", 32)
	if err := store.SavePi(context.Background(), gate, generation, 1, next); err != nil {
		t.Fatal(err)
	}
	next.Revision = 3
	next.Members[0].InitialInputID = strings.Repeat("e", 32)
	if err := store.SavePi(context.Background(), gate, generation, 2, next); !errors.Is(err, piruntime.ErrConflict) {
		t.Fatal("changed original initial input", err)
	}
}

type readyVerifierFunc func(context.Context, PiRoster) error

func (f readyVerifierFunc) VerifyPiRosterReady(ctx context.Context, r PiRoster) error {
	return f(ctx, r)
}
func TestPiRosterInitialInputAloneNeverPublishesReady(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	store, _ := NewStore(root, workspace)
	r := piRosterFixture(workspace)
	generation := strings.Repeat("e", 64)
	gate := &piWriteFake{session: r.Scope().Ref(), generation: generation}
	r.Members[0].State = "ready"
	r.Members[0].InitialInputID = strings.Repeat("f", 32)
	if err := store.SavePi(context.Background(), gate, generation, 0, r); !errors.Is(err, ErrPiUnavailable) {
		t.Fatal("optimistic input-only ready accepted", err)
	}
	if _, err := os.Stat(store.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ready refusal created domain locks")
	}
	verifier := readyVerifierFunc(func(_ context.Context, current PiRoster) error {
		if current.Members[0].Incarnation == r.Members[0].Incarnation {
			return piruntime.ErrHeld
		}
		return nil
	})
	if err := store.SavePi(context.Background(), gate, generation, 0, r, verifier); !errors.Is(err, piruntime.ErrHeld) {
		t.Fatal("stale/incomplete P2 proof accepted", err)
	}
}
