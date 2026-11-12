package monitoroperation

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

type piRecordGate struct {
	scope      agentbackend.SessionRef
	generation string
}

func (g piRecordGate) WithPiTeamWrite(ctx context.Context, s agentbackend.SessionRef, generation string, run func() error) error {
	if s != g.scope || generation != g.generation {
		return piruntime.ErrScope
	}
	return run()
}
func piOperationFixture(t *testing.T) (*Store, PiOperation, piRecordGate) {
	t.Helper()
	root, workspace := t.TempDir(), t.TempDir()
	store, err := NewStore(root, workspace)
	if err != nil {
		t.Fatal(err)
	}
	s := PiSessionRecord{Workspace: workspace, Slug: "2026-11-12-fixture", Incarnation: strings.Repeat("a", 64)}
	lead := PiConversationRecord{Session: s, Backend: "pi", Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "lead-native"}
	owner := PiOwnerRecord{Session: s, Lead: lead, Generation: strings.Repeat("c", 64), OriginInput: PiInputRecord{Conversation: lead, OperationID: strings.Repeat("d", 32), Action: "send", Digest: strings.Repeat("e", 64)}}
	settings := PiSettingsRecord{Provider: "fixture", AccountRef: "account", Model: "saved-model", Thinking: "low", PolicyRevision: strings.Repeat("f", 64), Revision: 1}
	policy := PiUtilityPolicyRecord{Kind: "verification_watcher", PackageRoot: "/nix/store/fixture-package", CatalogDigest: strings.Repeat("a", 64), Model: settings.Model, Effort: "low", Instructions: "Monitor exact source only.", SavedAccess: "workspace_write", ExecutionAccess: "full_access", MaxConcurrent: 1, Settings: settings}
	id := strings.Repeat("1", 32)
	req := StartRequest{ID: id, Brief: "Run the exact fixture command.", Source: Source{Kind: "local", Argv: []string{"check"}, Cwd: workspace, InputIdentity: "frozen-source", Revision: "tested-head", InvocationID: id}}
	op, err := NewPiOperation(req, owner, policy, strings.Repeat("2", 64), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return store, op, piRecordGate{scope: s.Ref(), generation: owner.Generation}
}
func TestPiOperationCodecLegacyAndOwnerSeparation(t *testing.T) {
	_, op, _ := piOperationFixture(t)
	data, _ := json.Marshal(op)
	decoded, err := DecodePiOperation(data)
	if err != nil {
		t.Fatal(err)
	}
	round, _ := json.Marshal(decoded)
	if string(data) != string(round) {
		t.Fatal("Pi codec changed values")
	}
	for _, bad := range []string{strings.Replace(string(data), `"backend":"pi"`, `"backend":"pi","socket":"fake-codex"`, 1), strings.Replace(string(data), `"schema":2`, `"schema":2,"schema":1`, 1), strings.Replace(string(data), `"originInput":{`, `"originInput":null,"bad":{`, 1), string(data) + `{}`} {
		if _, err := DecodePiOperation([]byte(bad)); err == nil {
			t.Fatal("accepted mixed/ambiguous record")
		}
	}
	legacy, _, req, _ := operationFixture(t)
	old, err := legacy.Start(context.Background(), "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	oldBytes, _ := json.Marshal(old)
	var reread Operation
	if err := json.Unmarshal(oldBytes, &reread); err != nil {
		t.Fatal(err)
	}
	same, _ := json.Marshal(reread)
	if string(oldBytes) != string(same) {
		t.Fatal("schema1 changed values/field order")
	}
	var candidate legacyOperation
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if d.Decode(&candidate) == nil && validateOperation(Operation(candidate)) == nil {
		t.Fatal("legacy schema1 reader accepted Pi")
	}
}
func TestPiOperationAdmissionIntentAndOriginalIDs(t *testing.T) {
	store, op, gate := piOperationFixture(t)
	ctx := context.Background()
	if _, err := store.ReservePi(ctx, nil, gate.generation, op); !errors.Is(err, piruntime.ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing gate created state")
	}
	for i := 0; i < 2; i++ {
		saved, err := store.ReservePi(ctx, gate, gate.generation, op)
		if err != nil || saved.Launch.CreationID != op.Launch.CreationID {
			t.Fatal(err)
		}
	}
	if err := store.UpdatePi(ctx, gate, op.Owner.Session, strings.Repeat("0", 64), op.ID, func(*PiOperation) error { t.Fatal("stale gate invoked callback"); return nil }); !errors.Is(err, piruntime.ErrScope) {
		t.Fatal(err)
	}
	tx, err := store.Lock(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Save(); !errors.Is(err, ErrPiUtilityUnavailable) {
		t.Fatal("public legacy tx bypassed gate", err)
	}
	tx.Close()
	changed := op
	changed.UtilityPolicy.Model = "another-model"
	if _, err := store.ReservePi(ctx, gate, gate.generation, changed); err == nil {
		t.Fatal("retry changed original policy")
	}
}

func TestPiUtilityReservationOmitsUnknownNativeIdentity(t *testing.T) {
	_, op, _ := piOperationFixture(t)
	data, _ := json.Marshal(op)
	for _, bad := range []string{strings.Replace(string(data), `"createAttempted":false,`, "", 1), strings.Replace(string(data), `"utility":{"session":`, `"utility":{"nativeId":"","session":`, 1)} {
		if _, err := DecodePiOperation([]byte(bad)); err == nil {
			t.Fatal("missing intent/empty native identity accepted")
		}
	}
}

type piEffectVerifierFunc func(context.Context, piruntime.RuntimeRef, PiOperation, string) error

func (f piEffectVerifierFunc) VerifyPiUtilityEffect(ctx context.Context, r piruntime.RuntimeRef, p PiOperation, a string) error {
	return f(ctx, r, p, a)
}
func TestPiUtilityCreateIntentPrecedesEffectAndNoMonitorLockCrossesCallback(t *testing.T) {
	store, op, gate := piOperationFixture(t)
	ctx := context.Background()
	if _, err := store.ReservePi(ctx, gate, gate.generation, op); err != nil {
		t.Fatal(err)
	}
	runtime := piruntime.RuntimeRef{Session: op.Owner.Session.Ref(), Generation: gate.generation}
	calls := 0
	verifier := piEffectVerifierFunc(func(_ context.Context, r piruntime.RuntimeRef, p PiOperation, action string) error {
		if r != runtime || p.ID != op.ID || action != "create" {
			return piruntime.ErrScope
		}
		return nil
	})
	authority := PiDomainAuthority{Store: store, Admission: gate, Verifier: verifier}
	err := authority.WithUtility(ctx, runtime, op.Launch.Utility, "create", func(grant PiUtilityDomainGrant) error {
		calls++
		if grant.CreationID != op.Launch.CreationID || grant.LaunchOperationID != op.Launch.MessageID || grant.Input.Message != op.Launch.Envelope || grant.Input.Retry {
			t.Fatal("effect changed original create/input")
		}
		saved, err := store.Get(ctx, op.ID)
		if err != nil || !saved.Pi.Launch.CreateAttempted {
			t.Fatal("effect preceded intent or retained file lock", err)
		}
		return errors.New("lost native create reply")
	})
	if err == nil || calls != 1 {
		t.Fatal("lost effect became success")
	}
	if err := authority.WithUtility(ctx, runtime, op.Launch.Utility, "send", func(PiUtilityDomainGrant) error { t.Fatal("missing original input admitted send"); return nil }); !errors.Is(err, piruntime.ErrScope) && !errors.Is(err, piruntime.ErrHeld) {
		t.Fatal(err)
	}
	wrong := op.Launch.Utility
	wrong.Incarnation = strings.Repeat("9", 64)
	if err := authority.WithUtility(ctx, runtime, wrong, "create", func(PiUtilityDomainGrant) error { t.Fatal("wrong utility entered effect"); return nil }); err == nil {
		t.Fatal("cross-owner utility accepted")
	}
}

func TestPiMonitorOriginalCompactAndHyphenatedIDsStayDistinct(t *testing.T) {
	store, op, gate := piOperationFixture(t)
	op.UtilityPolicy.MaxConcurrent = 2
	ctx := context.Background()
	if _, err := store.ReservePi(ctx, gate, gate.generation, op); err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-1111-1111-111111111111"
	req := StartRequest{ID: id, Brief: op.Brief, Source: op.Source}
	req.Source.InvocationID = id
	next, err := NewPiOperation(req, op.Owner, op.UtilityPolicy, strings.Repeat("2", 64), op.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReservePi(ctx, gate, gate.generation, next); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{op.ID, id} {
		path, err := store.path(key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal("operation spelling was normalized", err)
		}
	}
}

func TestPiLifecycleCannotTreatAbsentCodexFieldsAsUtilityDrain(t *testing.T) {
	_, op, _ := piOperationFixture(t)
	wrapped := Operation{Schema: 2, ID: op.ID, Workspace: op.Workspace, Pi: &op}
	if err := (Lifecycle{}).ProveQuiescent(context.Background(), wrapped); !errors.Is(err, ErrPiUtilityUnavailable) {
		t.Fatal("Pi utility inherited legacy empty-thread quiescence", err)
	}
	if err := knownMonitorReceipts(wrapped); !errors.Is(err, ErrPiUtilityUnavailable) {
		t.Fatal("utility receipt entered legacy reader", err)
	}
}

type piSettlementFake struct{ value PiSettlementObservation }

type piUtilityReadFunc func(context.Context, PiUtilityRecord, string, string, func() error) error

func (f piUtilityReadFunc) WithPiUtilityRead(ctx context.Context, u PiUtilityRecord, input, generation string, read func() error) error {
	return f(ctx, u, input, generation, read)
}

func (f piSettlementFake) ObservePiSettlement(context.Context, PiOperation) (PiSettlementObservation, error) {
	return f.value, nil
}
func TestPiDurableTerminalReportPrecedesCallbackDrainAndCancelDoesNotSettle(t *testing.T) {
	store, op, gate := piOperationFixture(t)
	ctx := context.Background()
	if _, err := store.ReservePi(ctx, gate, gate.generation, op); err != nil {
		t.Fatal(err)
	}
	err := store.UpdatePi(ctx, gate, op.Owner.Session, gate.generation, op.ID, func(p *PiOperation) error {
		p.Launch.CreateAttempted = true
		p.Launch.Utility.NativeID = "native-utility"
		p.Launch.State = "running"
		p.Launch.Generation = gate.generation
		p.Launch.Input = &PiUtilityInputRecord{Utility: p.Launch.Utility, OperationID: p.Launch.MessageID, Action: "send", Digest: strings.Repeat("3", 64)}
		p.Launch.Outcome = "accepted"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(ctx, op.ID)
	caller := current.Pi.Launch.Utility
	service := PiReportService{Store: store, Admission: gate}
	exit := 0
	report := ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &exit, Evidence: []string{"source-receipt"}}
	at := time.Now().UTC()
	forged := caller
	forged.Incarnation = strings.Repeat("9", 64)
	if _, err := service.Report(ctx, op.Owner.Session, gate.generation, forged, current.Pi.Launch.MessageID, report, at); !errors.Is(err, piruntime.ErrScope) {
		t.Fatal("wrong utility incarnation reported", err)
	}
	if _, err := service.Report(ctx, op.Owner.Session, gate.generation, caller, strings.Repeat("8", 32), report, at); !errors.Is(err, piruntime.ErrScope) {
		t.Fatal("wrong original input reported", err)
	}
	first, err := service.Report(ctx, op.Owner.Session, gate.generation, caller, current.Pi.Launch.MessageID, report, at)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Report(ctx, op.Owner.Session, gate.generation, caller, current.Pi.Launch.MessageID, report, at.Add(time.Second))
	if err != nil || first != second {
		t.Fatal("duplicate report slot changed", err)
	}
	proof := piSettlementFake{PiSettlementObservation{SourceTerminal: true, InputKnown: true, ResourcesDrained: true, ToolsDrained: true, UtilityExited: true, CompleteInventory: true, SourceEvidence: []string{"command-exit"}, ToolEvidence: []string{"tool-registry"}}}
	if err := service.ReconcileSettlement(ctx, op.Owner.Session, gate.generation, op.ID, proof, at); err != nil {
		t.Fatal(err)
	}
	held, _ := store.Get(ctx, op.ID)
	if held.Pi.Settlement.State != "incomplete" || len(held.Pi.Events) != 1 {
		t.Fatal("durable report implied callback drain")
	}
	statusCalls := 0
	service.ReadAdmission = piUtilityReadFunc(func(_ context.Context, u PiUtilityRecord, input, generation string, read func() error) error {
		statusCalls++
		if u != caller || input != current.Pi.Launch.MessageID || generation != gate.generation {
			return piruntime.ErrScope
		}
		return read()
	})
	beforeRead, _ := json.Marshal(held.Pi)
	observed, err := service.Status(ctx, caller, current.Pi.Launch.MessageID, gate.generation)
	if err != nil || observed.Settlement.State != "incomplete" || statusCalls != 1 {
		t.Fatal("held original status required delivery/idle authority", err)
	}
	afterRead, _ := store.Get(ctx, op.ID)
	afterBytes, _ := json.Marshal(afterRead.Pi)
	if string(beforeRead) != string(afterBytes) {
		t.Fatal("observational utility status mutated state")
	}
	if err := service.CancelPi(ctx, op.Owner.Session, gate.generation, op.ID, at); err != nil {
		t.Fatal(err)
	}
	canceled, _ := store.Get(ctx, op.ID)
	if canceled.Pi.Launch.Outcome != "accepted" || canceled.Pi.Launch.TerminatedAt != nil || canceled.Pi.Settlement.State != "incomplete" {
		t.Fatal("cancel cleared command/input holds")
	}
	// A distinct trusted observation, after all callbacks/resources exited,
	// can finish settlement; source exit alone above was insufficient.
	if err := store.UpdatePi(ctx, gate, op.Owner.Session, gate.generation, op.ID, func(p *PiOperation) error { p.Launch.State = "terminated"; p.Launch.TerminatedAt = &at; return nil }); err != nil {
		t.Fatal(err)
	}
	proof.value.CallbacksDrained = true
	if err := service.ReconcileSettlement(ctx, op.Owner.Session, gate.generation, op.ID, proof, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	final, _ := store.Get(ctx, op.ID)
	if final.Pi.Settlement.State != "settled" {
		t.Fatal("complete exact proof did not settle")
	}
}
