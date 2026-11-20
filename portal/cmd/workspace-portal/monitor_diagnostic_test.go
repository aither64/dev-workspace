package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
)

func diagnosticScopeFixture(t *testing.T) (monitorDiagnosticScope, *maintenanceUtilityClient) {
	t.Helper()
	m, _, _, native, _, _ := maintenanceMonitorFixture(t)
	runtime := t.TempDir()
	s := monitorDiagnosticScope{Registry: filepath.Join(t.TempDir(), "registry.json"), Name: "fixture", Workspace: m.workspace, State: m.store.Root,
		Runtime: runtime, Slug: "actor", Root: "00000000-0000-4000-8000-000000000001", Profile: filepath.Join(t.TempDir(), "profile"), Package: t.TempDir()}
	s.Socket, s.Authority = filepath.Join(runtime, s.Name, "app-server.sock"), filepath.Join(runtime, s.Name, "authority")
	registry, _ := json.Marshal(workspaceRegistry{Schema: 1, Workspaces: []workspaceBinding{{Name: s.Name, Root: s.Workspace, Hostname: "fixture.example"}}})
	if err := os.WriteFile(s.Registry, registry, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(s.Package, s.Profile); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: actor\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: " + s.Root + "\n  socket_path: " + s.Socket + "\n  client_version: 0.165.0\n"
	if err := os.WriteFile(filepath.Join(s.Workspace, "work", s.Slug, "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_WORKSPACES_CONFIG", s.Registry)
	t.Setenv("DEV_WORKSPACES_STATE", s.State)
	t.Setenv("DEV_WORKSPACES_RUNTIME_DIR", s.Runtime)
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", filepath.Join(s.State, "codex-homes", s.Name))
	return s, native
}

func (s monitorDiagnosticScope) args() []string {
	return []string{"--registry", s.Registry, "--workspace-name", s.Name, "--workspace", s.Workspace, "--user-state-root", s.State,
		"--runtime-dir", s.Runtime, "--session-slug", s.Slug, "--root-thread-id", s.Root, "--socket", s.Socket,
		"--authority-dir", s.Authority, "--host-profile", s.Profile, "--selected-package", s.Package}
}

func TestMonitorDiagnosticScopeBeforeClient(t *testing.T) {
	for _, fault := range []string{"registry", "name", "workspace", "state", "runtime", "slug", "root", "socket", "authority", "profile", "package", "native-home", "arguments"} {
		t.Run(fault, func(t *testing.T) {
			s, _ := diagnosticScopeFixture(t)
			if _, err := s.validate(); err != nil {
				t.Fatal("valid owning scope refused", err)
			}
			switch fault {
			case "registry":
				s.Registry += "-PRIVATE"
			case "name":
				s.Name = "foreign"
			case "workspace":
				s.Workspace = t.TempDir()
			case "state":
				s.State = t.TempDir()
			case "runtime":
				s.Runtime = t.TempDir()
			case "slug":
				s.Slug = "missing"
			case "root":
				s.Root = "00000000-0000-4000-8000-000000000002"
			case "socket":
				s.Socket += "-PRIVATE"
			case "authority":
				s.Authority += "-PRIVATE"
			case "profile":
				s.Profile += "-PRIVATE"
			case "package":
				s.Package = t.TempDir()
			case "native-home":
				t.Setenv("DEV_WORKSPACE_CODEX_HOME", "/PRIVATE")
			}
			args := s.args()
			if fault == "arguments" {
				args = append(args, "--PRIVATE=/PRIVATE")
			}
			var output bytes.Buffer
			err := monitorInventoryDiagnosticCommand(args, &output, func(string, string, string) (monitoroperation.LifecycleClient, func()) {
				t.Fatal("scope refusal constructed a native client")
				return nil, func() {}
			})
			if err == nil || output.Len() != 0 || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("scope refusal leaked content or admitted client", err)
			}
		})
	}
}

func TestMonitorDiagnosticCommandFailureAndScopeRecheck(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "proof-failure", true: "selection-changed"}[changed], func(t *testing.T) {
			s, native := diagnosticScopeFixture(t)
			before, mutations := maintenanceMonitorProofBytes(t, s.State), native.mutations
			var output bytes.Buffer
			closed, constructed := 0, 0
			err := monitorInventoryDiagnosticCommand(s.args(), &output, func(socket, workspace, state string) (monitoroperation.LifecycleClient, func()) {
				constructed++
				if socket != s.Socket || workspace != s.Workspace || state != s.State {
					t.Fatal("constructor scope changed")
				}
				if changed {
					if err := os.Remove(s.Profile); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(t.TempDir(), s.Profile); err != nil {
						t.Fatal(err)
					}
				}
				return native, func() { closed++ }
			})
			var result monitorDiagnosticResult
			if err == nil || json.Unmarshal(output.Bytes(), &result) != nil || constructed != 1 || closed != 1 || !result.Advisory ||
				!strings.Contains(result.Effects, "may write local storage") || result.Result == "success" || changed && result.Result != "scope-changed" {
				t.Fatal("lost refusal/close/disclosure", result, err)
			}
			if native.mutations != mutations || !reflect.DeepEqual(before, maintenanceMonitorProofBytes(t, s.State)) {
				t.Fatal("diagnostic changed monitor records/native effects")
			}
			if bytes.Contains(output.Bytes(), []byte(s.Workspace)) || bytes.Contains(output.Bytes(), []byte(s.Socket)) {
				t.Fatal("output contains paths")
			}
		})
	}
}

func TestMonitorDiagnosticProofTimingAndImmediateCause(t *testing.T) {
	for _, mode := range []string{"success", "identity", "deadline", "earlier-budget", "first-error-later-deadline"} {
		t.Run(mode, func(t *testing.T) {
			m, o, service, native, op, summary := maintenanceMonitorFixture(t)
			completeMaintenanceMonitorProof(t, service, native, op)
			completeMaintenanceObserverProof(t, service, op.Owner)
			before, mutations := maintenanceMonitorProofBytes(t, m.store.Root), native.mutations
			result := monitorDiagnosticResult{Stages: []monitorDiagnosticRow{}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			started := time.Now()
			var entryBudget time.Duration
			native.readHook = func(call context.Context, stage, id string) error {
				deadline, ok := call.Deadline()
				if !ok {
					t.Fatal("proof lost deadline")
				}
				if stage == "native-project" {
					entryBudget = time.Until(deadline)
				}

				if stage == "native-idle" {
					switch mode {
					case "deadline":
						<-call.Done()
						return call.Err()
					case "first-error-later-deadline":
						return &codex.TransportError{Err: errors.New("PRIVATE /path token header transcript")}
					}
				}
				return call.Err()
			}
			if mode == "earlier-budget" {
				cancel()
			}
			if mode == "identity" {
				native.cwd = "/PRIVATE wrong identity"
			}
			observe := result.observer(started, service.Store)
			ctx = monitoroperation.WithProofObserver(ctx, func(row monitoroperation.ProofObservation) {
				observe(row)
				if mode == "first-error-later-deadline" && row.Stage == "native-idle" {
					<-ctx.Done()
				}
			})
			_, err := maintenanceMonitorInventory(ctx, service.Store, summary, o.Services[0].Socket, native)
			if mode == "success" && err != nil || mode != "success" && err == nil {
				t.Fatal(mode, err)
			}
			if mode != "earlier-budget" && entryBudget <= 0 {
				t.Fatal("native proof entered without expected budget")
			}
			var stages []string
			var idle, thread *monitorDiagnosticRow
			for i := range result.Stages {
				row := &result.Stages[i]
				stages = append(stages, row.Stage)
				if row.DurationMS < 0 || row.ElapsedMS < 0 || row.RemainingMS > 1000 {
					t.Fatal("invalid timing", row)
				}
				if row.Stage == "native-idle" {
					idle = row
				}
				if row.Stage == "native-thread" {
					thread = row
				}
			}
			if mode == "success" {
				want := []string{"inventory", "inventory-list", "inventory-observers", "owner-identity", "owner-match", "local-list", "native-project", "native-thread", "native-bootstrap", "native-archive-state", "native-idle", "local-observers", "local-safety"}
				if !reflect.DeepEqual(stages, want) {
					t.Fatal("proof order changed", stages)
				}
			}
			if mode == "identity" && (thread == nil || thread.Result != "identity-mismatch") {
				t.Fatal("successful read mismatch lost", thread)
			}
			if mode == "deadline" && (idle == nil || idle.Result != "deadline" || idle.ContextAtEntry != "success" || idle.RemainingMS <= 0 || !errors.Is(err, context.DeadlineExceeded)) {
				t.Fatal("deadline attribution lost", idle, err)
			}
			if mode == "earlier-budget" && (len(result.Stages) != 1 || result.Stages[0].ContextAtEntry != "canceled") {
				t.Fatal("entry context lost", result)
			}
			if mode == "first-error-later-deadline" && (idle == nil || idle.Result != "transport" || !errors.Is(err, context.DeadlineExceeded)) {
				t.Fatal("later error replaced immediate failure", idle, err)
			}
			data, _ := json.Marshal(result)
			if bytes.Contains(data, []byte("PRIVATE")) || native.mutations != mutations || !reflect.DeepEqual(before, maintenanceMonitorProofBytes(t, m.store.Root)) {
				t.Fatal("observation leaked or mutated state")
			}
		})
	}
}

func TestMonitorDiagnosticOutputBound(t *testing.T) {
	store, err := monitoroperation.NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result := monitorDiagnosticResult{}
	observe := result.observer(time.Now(), store)
	for i := 0; i < 1000; i++ {
		observe(monitoroperation.ProofObservation{Stage: "native-thread", MonitorID: "PRIVATE unrelated identity", EntryContext: "success", Result: "other", Started: time.Now()})
	}
	data, err := json.Marshal(result)
	if err != nil || len(data) >= 64*1024 || len(result.Stages) != 128 || result.Observed != 1000 || !result.Truncated || bytes.Contains(data, []byte("PRIVATE")) {
		t.Fatal("unbounded or identifying output", len(data), err)
	}
}

func TestMonitorDiagnosticColdFiveSecondBudget(t *testing.T) {
	_, o, service, native, op, summary := maintenanceMonitorFixture(t)
	completeMaintenanceMonitorProof(t, service, native, op)
	native.readHook = func(ctx context.Context, stage, _ string) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) < 4*time.Second {
			t.Fatal("maintenance five-second budget changed", stage)
		}
		return ctx.Err()
	}
	if _, err := maintenanceMonitorInventory(monitoroperation.WithProofObserver(context.Background(), func(monitoroperation.ProofObservation) {}), service.Store, summary, o.Services[0].Socket, native); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorDiagnosticCommandEmptySuccess(t *testing.T) {
	s, native := diagnosticScopeFixture(t)
	s.State = t.TempDir()
	t.Setenv("DEV_WORKSPACES_STATE", s.State)
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", filepath.Join(s.State, "codex-homes", s.Name))
	before := native.mutations
	native.reads = nil // Exclude disposable monitor setup's identity reads.
	var output bytes.Buffer
	closed := false
	err := monitorInventoryDiagnosticCommand(s.args(), &output, func(string, string, string) (monitoroperation.LifecycleClient, func()) {
		return native, func() { closed = true }
	})
	var result monitorDiagnosticResult
	if err != nil || json.Unmarshal(output.Bytes(), &result) != nil || result.Result != "success" || !closed || native.mutations != before || len(native.reads) != 0 {
		t.Fatal("empty inventory changed effects", result, err)
	}
	entries, err := os.ReadDir(s.State)
	if err != nil || len(entries) != 0 {
		t.Fatal("empty diagnostic created local records", err)
	}
}

func TestMonitorDiagnosticCommandOutputStreams(t *testing.T) {
	s, _ := diagnosticScopeFixture(t)
	s.State = t.TempDir() // Empty real store; the real cold client needs no RPC.
	t.Setenv("DEV_WORKSPACES_STATE", s.State)
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", filepath.Join(s.State, "codex-homes", s.Name))
	t.Setenv("DEV_WORKSPACE_DELIVERY_TRACE", "1")
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	previousOut, previousErr, previousLog := os.Stdout, os.Stderr, log.Writer()
	os.Stdout, os.Stderr = stdout, stderr
	log.SetOutput(stderr)
	defer func() { os.Stdout, os.Stderr = previousOut, previousErr; log.SetOutput(previousLog) }()
	err = monitorLifecycleCommand(append([]string{"diagnose-inventory"}, s.args()...))
	out, readErr := os.ReadFile(stdout.Name())
	errOut, readErr2 := os.ReadFile(stderr.Name())
	var result monitorDiagnosticResult
	if err != nil || readErr != nil || readErr2 != nil || json.Unmarshal(out, &result) != nil || result.Result != "success" || len(errOut) != 0 || len(out)+len(errOut) > 64*1024 {
		t.Fatal("command stream contract failed", err)
	}
	for _, private := range []string{s.Workspace, s.Root, s.Socket, s.State} {
		if bytes.Contains(out, []byte(private)) || bytes.Contains(errOut, []byte(private)) {
			t.Fatal("command stream leaked an identity/path")
		}
	}
}

// This is the actual pinned C ledger, not a no-op LifecycleClient. A nonexistent
// socket makes any unexpected RPC fail; empty resolved ledgers need no RPC.
func TestMonitorDiagnosticPinnedLedgerBookkeeping(t *testing.T) {
	root := t.TempDir()
	var diagnosticLog bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&diagnosticLog)
	t.Cleanup(func() { log.SetOutput(previousLog) })
	t.Setenv("DEV_WORKSPACE_DELIVERY_TRACE", "1")
	socket, workspace := filepath.Join(root, "absent.sock"), t.TempDir()
	identity := sha256.Sum256([]byte(socket))
	base := filepath.Join(root, "submissions", hex.EncodeToString(identity[:]))
	primary, legacy, database := base+".json", socket+".submission-attempts-v3.json", base+".sqlite"
	if err := os.WriteFile(legacy, []byte(`{"schema":3,"attempts":{},"sends":{"other-disposable-thread":{"00000000-0000-4000-8000-000000000009":{"digest":"`+strings.Repeat("a", 64)+`","state":"prepared","steered":false}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"absent-import", "ready-present", "missing-fence"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "missing-fence" {
				if err := os.Remove(primary); err != nil {
					t.Fatal(err)
				}
			}
			client := configuredCodexClientObserved(socket, workspace, nil, true, nil, root)
			// Construction is lazy even when the proof will initialize storage.
			if phase == "absent-import" {
				if _, err := os.Stat(database); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("constructor opened database", err)
				}
			}
			err := client.RequireSubmissionAttemptsResolved(context.Background(), "disposable-thread")
			client.Close()
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{primary, legacy} {
				data, err := os.ReadFile(path)
				var fence struct {
					Schema            int `json:"schema"`
					Storage, Database string
				}
				if err != nil || json.Unmarshal(data, &fence) != nil || fence.Schema != 4 || fence.Storage != "sqlite" || fence.Database != database {
					t.Fatal("ordinary proof did not retain/restore fence", err)
				}
			}
			if info, err := os.Stat(database); err != nil || info.Size() == 0 || info.Mode().Perm() != 0600 {
				t.Fatal("database bookkeeping absent", err)
			}
			db, err := sql.Open("sqlite", database)
			if err != nil {
				t.Fatal(err)
			}
			var count int
			var state string
			err = db.QueryRow("SELECT count(*),min(state) FROM submission_records").Scan(&count, &state)
			db.Close()
			if err != nil || count != 1 || state != "prepared" || diagnosticLog.Len() != 0 {
				t.Fatal("diagnostic reconciled a receipt or leaked ordinary delivery logging", count, state, err)
			}
			t.Log("pinned ledger proof completed; database/import/fence bookkeeping is allowed, no native socket exists")
		})
	}
}
