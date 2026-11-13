package piruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestReadStartIsExactAndMissingDoesNotCreateRuntime(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	if _, err := m.ReadStart(ctx, start.Session); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	directory, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created state", err)
	}
	if h.count != 0 {
		t.Fatal("read called native")
	}
	if _, err := m.Ensure(ctx, start); err != nil {
		t.Fatal(err)
	}
	saved, err := m.ReadStart(ctx, start.Session)
	if err != nil || saved != start || h.count != 1 {
		t.Fatal(saved, err)
	}
	caller := start
	caller.BindingRevision++
	if _, err := m.Ensure(ctx, caller); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if current, err := m.ReadStart(ctx, start.Session); err != nil || current != start || h.count != 1 {
		t.Fatal("read replay changed start", current, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.ReadStart(canceled, start.Session); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestReadStartMissingRecordHoldsArtifactsWithoutNativeEffects(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	directory, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	if err := makePrivateDirectory(directory); err != nil {
		t.Fatal(err)
	}
	checkAbsent := func() {
		t.Helper()
		if _, err := m.ReadStart(ctx, start.Session); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("fresh directory refused", err)
		}
	}
	checkHeld := func() {
		t.Helper()
		if _, err := m.ReadStart(ctx, start.Session); !errors.Is(err, ErrHeld) || errors.Is(err, os.ErrNotExist) {
			t.Fatal("ambiguous artifacts admitted as fresh", err)
		}
	}
	checkAbsent()
	lock := filepath.Join(directory, "runtime.lock")
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	checkAbsent()
	if err := os.WriteFile(lock, []byte("unexpected"), 0600); err != nil {
		t.Fatal(err)
	}
	checkHeld()
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", lock); err != nil {
		t.Fatal(err)
	}
	checkHeld()
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "members"), 0700); err != nil {
		t.Fatal(err)
	}
	checkHeld()
	if h.count != 0 {
		t.Fatal("ReadStart invoked native host")
	}
	if _, err := os.Stat(filepath.Join(directory, "runtime.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ReadStart repaired missing record", err)
	}
}
func TestRetainedCreationPreservesLiteral64HexID(t *testing.T) {
	m, _, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	creation := strings.Repeat("d", 64)
	state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: creation, Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	proof, err := m.VerifyCreation(ctx, agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}, creation, state.HistoryRef, nil)
	if err != nil || proof.CreationID != creation {
		t.Fatal(proof, err)
	}
	for _, bad := range []string{strings.Repeat("D", 64), strings.Repeat("d", 63), "../" + creation} {
		if creationPattern.MatchString(bad) {
			t.Fatal("invalid creation alias", bad)
		}
	}
}
func TestFixedSupervisorManifestValidationHasNoNativeEffects(t *testing.T) {
	valid := `{"schema":1,"host_launcher_contract":1,"launch_request_schema":1,"handshake_schema":1,"supervisor_entrypoint":"bin/workspace-portal","supervisor_command":["pi-runtime","supervise"]}`
	if err := validateSupervisorManifest([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(valid, `"schema":1`, `"schema":2`, 1), strings.Replace(valid, `"schema":1`, `"schema":1,"schema":1`, 1), strings.Replace(valid, `"schema":1`, `"schema":null`, 1), strings.Replace(valid, `"supervise"`, `"arbitrary-command"`, 1), valid + `{}`} {
		if validateSupervisorManifest([]byte(bad)) == nil {
			t.Fatal("invalid H0 application manifest")
		}
	}
	if ValidatePackage("/tmp/caller-selected-package") == nil {
		t.Fatal("mutable package accepted")
	}
}
