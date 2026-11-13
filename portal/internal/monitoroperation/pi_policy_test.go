package monitoroperation

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func piUtilityFixture() PiUtilityLaunch {
	id := "11111111111111111111111111111111"
	return PiUtilityLaunch{
		Handle: PiUtilityHandle{Session: agentbackend.SessionRef{Workspace: "/workspace", Slug: "2026-11-12-fixture", Incarnation: strings.Repeat("a", 64)},
			OperationID: id, Incarnation: strings.Repeat("b", 64), NativeID: "utility-native", Generation: strings.Repeat("c", 64)},
		LaunchOperationID: "22222222222222222222222222222222", InputReceiptID: "receipt", CreationProof: "persisted-creation",
		Policy:  PiUtilityPolicy{PackageRoot: "/nix/store/fixture", CatalogDigest: strings.Repeat("d", 64), Model: "saved-model", Effort: "low", Instructions: "Monitor only.", SavedAccess: "workspace_write", ExecutionAccess: "full_access", MaxConcurrent: 1},
		Request: StartRequest{ID: id, Brief: "Observe the exact test.", Source: Source{Kind: "local", Argv: []string{"test", "--exact"}, Cwd: "/workspace", InputIdentity: "input-hash", Revision: "source-head", InvocationID: id}},
	}
}

func TestPiUtilityCannotLaunchThroughRetainedMemberMode(t *testing.T) {
	launch := piUtilityFixture()
	before := launch
	if err := CheckPiUtilityLaunch(launch); !errors.Is(err, ErrPiUtilityUnavailable) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, launch) {
		t.Fatal("refusal changed launch/source/policy")
	}
	// There is no caller-selected member address, retained Create/Bind handle,
	// process callback or writer in this API.
	if err := CheckPiUtilityRetry(launch, launch); err != nil {
		t.Fatal(err)
	}
	retry := launch
	retry.Policy.Model = "other-model"
	if !errors.Is(CheckPiUtilityRetry(launch, retry), piruntime.ErrConflict) {
		t.Fatal("retry changed frozen policy")
	}
	retry = launch
	retry.LaunchOperationID = "33333333333333333333333333333333"
	if !errors.Is(CheckPiUtilityRetry(launch, retry), piruntime.ErrConflict) {
		t.Fatal("retry replaced original operation")
	}
}

func TestPiUtilityRequiresManagedPolicyInsteadOfInventedDefaults(t *testing.T) {
	if _, err := PiPolicyFromCatalog("/missing-package", nil, nil); err == nil {
		t.Fatal("unmanaged utility silently selected defaults")
	}
}

func TestPiUtilityTerminalNeedsSettlementAndImmutableReportSlot(t *testing.T) {
	h := piUtilityFixture().Handle
	status := 0
	report := ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &status, Evidence: []string{"exact-log"}}
	for _, settlement := range []PiUtilitySettlement{{SourceComplete: true}, {SourceComplete: true, EffectsSettled: true}} {
		if !errors.Is(CheckPiUtilityReport(h, h, nil, report, settlement), piruntime.ErrHeld) {
			t.Fatal("incomplete effects/publication became terminal success")
		}
	}
	settled := PiUtilitySettlement{SourceComplete: true, EffectsSettled: true, ReportPublished: true}
	if err := CheckPiUtilityReport(h, h, &report, report, settled); err != nil {
		t.Fatal(err)
	}
	changed := report
	changed.Detail = "changed report"
	if !errors.Is(CheckPiUtilityReport(h, h, &report, changed, settled), piruntime.ErrConflict) {
		t.Fatal("slot payload changed")
	}
	stale := h
	stale.Generation = strings.Repeat("f", 64)
	if !errors.Is(CheckPiUtilityReport(h, stale, nil, report, settled), piruntime.ErrScope) {
		t.Fatal("stale utility caller accepted")
	}
	incomplete := ReportRequest{Slot: "terminal", Outcome: "incomplete", Detail: "command/cell outcome unknown", StillRunning: "cell-1"}
	if err := CheckPiUtilityReport(h, h, nil, incomplete, PiUtilitySettlement{}); err != nil {
		t.Fatal(err)
	}
}
