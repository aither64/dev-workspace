package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

const monitorDiagnosticRows = 128

type monitorDiagnosticRow struct {
	Stage          string  `json:"stage"`
	Method         string  `json:"method"`
	Monitor        int     `json:"monitor,omitempty"`
	ElapsedMS      float64 `json:"elapsedMs"`
	DurationMS     float64 `json:"durationMs"`
	RemainingMS    float64 `json:"remainingMs"`
	ContextAtEntry string  `json:"contextAtEntry"`
	Result         string  `json:"result"`
	RecordReads    uint64  `json:"recordReads"`
}

type monitorDiagnosticResult struct {
	Advisory       bool                   `json:"advisory"`
	Session        string                 `json:"session"`
	SourceRevision string                 `json:"sourceRevision,omitempty"`
	SourceModified bool                   `json:"sourceModified"`
	CodexWeb       string                 `json:"codexWeb,omitempty"`
	Effects        string                 `json:"effects"`
	Result         string                 `json:"result"`
	ElapsedMS      float64                `json:"elapsedMs"`
	RecordReads    uint64                 `json:"recordReads"`
	Observed       int                    `json:"observedStages"`
	Truncated      bool                   `json:"truncated"`
	Stages         []monitorDiagnosticRow `json:"stages"`
}

// Scope is supplied by the selected host, never by a foreign session shell.
// Registry/manifest readers and the host's existing path formulas own the join.
type monitorDiagnosticScope struct {
	Registry, Name, Workspace, State, Runtime, Slug, Root, Socket, Authority, Profile, Package string
}

func (s monitorDiagnosticScope) validate() (*session.Summary, error) {
	for _, path := range []string{s.Registry, s.Workspace, s.State, s.Runtime, s.Socket, s.Authority, s.Profile, s.Package} {
		if !canonicalAbsolutePath(path) {
			return nil, errors.New("diagnostic requires exact selected host coordinates")
		}
	}
	if !workspaceNamePattern.MatchString(s.Name) || !session.ValidSlug(s.Slug) || !teamMCPThreadIDPattern.MatchString(s.Root) ||
		os.Getenv("DEV_WORKSPACES_CONFIG") != s.Registry || os.Getenv("DEV_WORKSPACES_STATE") != s.State ||
		os.Getenv("DEV_WORKSPACES_RUNTIME_DIR") != s.Runtime ||
		os.Getenv("DEV_WORKSPACE_CODEX_HOME") != filepath.Join(s.State, "codex-homes", s.Name) ||
		s.Socket != filepath.Join(s.Runtime, s.Name, "app-server.sock") ||
		s.Authority != filepath.Join(s.Runtime, s.Name, "authority") {
		return nil, errors.New("diagnostic host selection does not match exact scope")
	}
	selected, err := filepath.EvalSymlinks(s.Profile)
	if err != nil || selected != s.Package {
		return nil, errors.New("diagnostic selected profile changed")
	}
	registry, err := loadWorkspaceRegistry(s.Registry)
	if err != nil {
		return nil, errors.New("diagnostic workspace registration is unavailable")
	}
	registered := false
	for _, binding := range registry.Workspaces {
		registered = registered || binding.Name == s.Name && binding.Root == s.Workspace
	}
	if !registered {
		return nil, errors.New("diagnostic workspace registration does not match")
	}
	summary, err := session.Find(s.Workspace, s.Slug)
	if err != nil || summary.Archived || summary.Codex.ThreadID != s.Root || session.RequireRetainedReady(summary, s.Socket) != nil {
		return nil, errors.New("diagnostic retained session scope is unavailable")
	}
	if pending, err := session.PendingLifecycle(s.Workspace, s.Slug); err != nil || pending != "" {
		return nil, errors.New("diagnostic session has pending lifecycle work")
	}
	return summary, nil
}

func monitorInventoryDiagnosticCommand(args []string, output io.Writer, construct func(string, string, string) (monitoroperation.LifecycleClient, func())) error {
	s := monitorDiagnosticScope{}
	f := flag.NewFlagSet("monitor-lifecycle diagnose-inventory", flag.ContinueOnError)
	// Parser errors can contain caller-supplied paths; expose only a fixed refusal.
	f.SetOutput(io.Discard)
	f.StringVar(&s.Registry, "registry", "", "selected host registry")
	f.StringVar(&s.Name, "workspace-name", "", "registered workspace name")
	f.StringVar(&s.Workspace, "workspace", "", "canonical workspace")
	f.StringVar(&s.State, "user-state-root", "", "selected private state root")
	f.StringVar(&s.Runtime, "runtime-dir", "", "selected runtime root")
	f.StringVar(&s.Slug, "session-slug", "", "named read scope")
	f.StringVar(&s.Root, "root-thread-id", "", "retained root")
	f.StringVar(&s.Socket, "socket", "", "selected Codex socket")
	f.StringVar(&s.Authority, "authority-dir", "", "selected runtime authority directory")
	f.StringVar(&s.Profile, "host-profile", "", "selected profile link")
	f.StringVar(&s.Package, "selected-package", "", "expected profile target")
	if f.Parse(args) != nil || f.NArg() != 0 {
		return errors.New("invalid inventory diagnostic arguments")
	}
	summary, err := s.validate()
	if err != nil {
		return err // All validation errors are fixed, path/content-free refusals.
	}
	store, err := monitoroperation.NewStore(s.State, s.Workspace)
	if err != nil {
		return errors.New("diagnostic store scope is unavailable")
	}
	client, closeClient := construct(s.Socket, s.Workspace, s.State)
	defer closeClient()
	started := time.Now()
	result := monitorDiagnosticResult{Advisory: true, Session: s.Slug, Stages: []monitorDiagnosticRow{},
		Effects: "ordinary submission database initialization/import/WAL/fence bookkeeping may write local storage; no reconciliation or native/lifecycle actions"}
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			if setting.Key == "vcs.revision" {
				result.SourceRevision = setting.Value
			}
			if setting.Key == "vcs.modified" {
				result.SourceModified = setting.Value == "true"
			}
		}
		for _, dependency := range build.Deps {
			if dependency.Path == "github.com/aither64/codex-web" {
				result.CodexWeb = dependency.Version
			}
		}
	}
	observer := result.observer(started, store)
	ctx := monitoroperation.WithProofObserver(context.Background(), observer)
	_, proofErr := maintenanceMonitorInventory(ctx, store, *summary, s.Socket, client)
	result.Result = monitoroperation.ProofErrorClass(proofErr)
	// Advisory measurement refuses changed tracking/profile selection; no repair.
	after, scopeErr := s.validate()
	if scopeErr != nil || !reflect.DeepEqual(summary, after) {
		result.Result = "scope-changed"
		proofErr = errors.New("diagnostic scope changed during measurement")
	}
	result.ElapsedMS = float64(time.Since(started)) / float64(time.Millisecond)
	result.RecordReads = store.RecordReads()
	data, err := json.Marshal(result)
	if err != nil || len(data)+1 > 64*1024 {
		return errors.New("diagnostic output exceeds its bound")
	}
	if _, err := output.Write(append(data, '\n')); err != nil {
		return errors.New("diagnostic output could not be written")
	}
	if proofErr != nil {
		return errors.New("inventory diagnostic proof refused; see sanitized result")
	}
	return nil
}

func (result *monitorDiagnosticResult) observer(started time.Time, store *monitoroperation.Store) monitoroperation.ProofObserver {
	ordinals := map[string]int{}
	return func(row monitoroperation.ProofObservation) {
		result.Observed++
		if len(result.Stages) >= monitorDiagnosticRows {
			result.Truncated = true
			return // Observation bounds never stop or shorten the proof.
		}
		ordinal := 0
		if row.MonitorID != "" {
			ordinal = ordinals[row.MonitorID]
			if ordinal == 0 {
				ordinal = len(ordinals) + 1
				ordinals[row.MonitorID] = ordinal
			}
		}
		result.Stages = append(result.Stages, monitorDiagnosticRow{Stage: row.Stage, Method: monitorDiagnosticMethod(row.Stage), Monitor: ordinal,
			ElapsedMS: float64(row.Started.Sub(started)) / float64(time.Millisecond), DurationMS: float64(row.Duration) / float64(time.Millisecond),
			RemainingMS: float64(row.Remaining) / float64(time.Millisecond), ContextAtEntry: row.EntryContext, Result: row.Result, RecordReads: store.RecordReads()})
	}
}

func monitorDiagnosticMethod(stage string) string {
	switch stage {
	case "inventory":
		return "Store.Inventory"
	case "inventory-list", "local-list":
		return "Store.List"
	case "inventory-observers", "local-observers":
		return "Store.ListObservers"
	case "owner-identity":
		return "IdentityFor"
	case "owner-match":
		return "SameOwner"
	case "local-safety":
		return "Lifecycle.AssessLocalSafety"
	case "native-project":
		return "ReadProject"
	case "native-thread":
		return "ReadThreadMetadata(true)"
	case "native-materialized":
		return "HeadlessThreadMaterialized"
	case "native-bootstrap":
		return "VerifyHeadlessBootstrap"
	case "native-archive-state":
		return "ProveArchivedThread"
	case "prepared-receipt":
		return "IsPreparedSendWithOptions"
	case "native-prepared-idle":
		return "ObserveThreadIdentityExceptPreparedSend"
	case "native-idle":
		return "RequireArchiveThreadIdle"
	}
	return "unknown"
}
