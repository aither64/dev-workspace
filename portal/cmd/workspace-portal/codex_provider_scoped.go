package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/aither64/dev-workspace/portal/internal/web"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"golang.org/x/sys/unix"
)

// The fork worker freezes one accepted operation and uses same-package writers.
type providerForkSource struct {
	Current        codexproviders.CurrentBinding   `json:"current"`
	Thread         providerThread                  `json:"thread"`
	Title          string                          `json:"title"`
	History        providerForkHistory             `json:"history"`
	BrowserRecords *web.ProviderForkBrowserRecords `json:"browserRecords"`
}
type providerForkHistory struct {
	Turns []json.RawMessage `json:"turns"`
	Items []json.RawMessage `json:"items"`
}
type providerForkRoster struct {
	Root                string `json:"rootThreadId"`
	Incarnation         string `json:"incarnation"`
	Revision            uint64 `json:"revision"`
	BindingOriginSchema int    `json:"bindingOriginSchema"`
}
type providerForkSnapshot struct {
	Schema              int                            `json:"schema"`
	Strategy            string                         `json:"strategy"`
	OperationID         string                         `json:"operationId"`
	Workspace           string                         `json:"workspace"`
	Root                string                         `json:"root"`
	Socket              string                         `json:"socket"`
	CatalogDigest       string                         `json:"catalogDigest"`
	SourcePackage       string                         `json:"sourcePackage"`
	PreviousStateDigest string                         `json:"previousStateDigest"`
	State               codexproviders.State           `json:"state"`
	Selection           codexproviders.SelectionChange `json:"selection"`
	Sessions            []string                       `json:"sessions"`
	SocketIdentity      string                         `json:"socketIdentity"`
	Rosters             map[string]providerForkRoster  `json:"rosters"`
	Participants        []providerForkSource           `json:"participants"`
}
type providerForkAdmission struct {
	State     codexproviders.State
	Authority string
	Required  map[string]bool // Supplied by the owning manifest reader, never inferred from absence.
	Currents  map[string]codexproviders.CurrentBinding
	Rosters   map[string]providerForkRoster
}

func (a *providerForkAdmission) resolve(ctx context.Context, thread providerThread, store *teamruntime.Store) (providerThread, error) {
	keyRoot := a.State.Operation.Roots[thread.Slug]
	required, present := a.Required[thread.Slug]
	authority, err := session.LoadRuntimeAuthority(a.Authority, thread.Slug, a.State.Root)
	if err != nil || !present || authority.CodexThreadID != keyRoot || authority.CodexSocketPath != a.State.Socket || len(authority.TmuxIdentity) != 64 {
		return thread, errors.New("fork origin/runtime incarnation is unavailable")
	}
	roster, err := store.Load(thread.Slug, keyRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return thread, err
	}
	revision := uint64(0)
	member := "lead"
	if roster != nil {
		revision = roster.Revision
	}
	if thread.MemberAddress != "" {
		member = thread.MemberAddress
		found := false
		if roster != nil {
			for _, m := range roster.Members {
				if m.Address == member && m.Thread == thread.ID && m.State == "ready" && m.RetireIntent == "" {
					found = true
				}
			}
		}
		if !found {
			return thread, errors.New("fork member is no longer current")
		}
	} else if thread.ID != keyRoot {
		return thread, errors.New("fork lead origin changed")
	}
	summary, err := session.Find(a.State.Root, thread.Slug)
	if err != nil || summary.Codex.ThreadID != keyRoot || required != (summary.Codex.BindingSchema == 3) {
		return thread, errors.New("fork manifest origin is unavailable")
	}
	origin := summary.Schema
	if origin == 6 {
		origin = summary.Codex.BindingOriginSchema
	}
	if origin < 1 || origin > 3 {
		return thread, errors.New("fork manifest origin schema is invalid")
	}
	frozen := providerForkRoster{keyRoot, authority.TmuxIdentity, revision, origin}
	if old, exists := a.Rosters[thread.Slug]; exists && old != frozen {
		return thread, errors.New("fork roster changed under ownership")
	}
	a.Rosters[thread.Slug] = frozen
	expected := uint64(0)
	if b, ok := a.State.Bindings[thread.Slug]; ok {
		expected = b.Revision
	}
	current := codexproviders.CurrentBinding{Key: codexproviders.BindingKey{Workspace: a.State.Root, Slug: thread.Slug, Incarnation: authority.TmuxIdentity, RootThreadID: keyRoot, Member: member, OriginalThreadID: thread.ID}, BindingRequired: required, ExpectedRevision: expected, Provider: a.State.Provider(thread.Slug, keyRoot), Socket: a.State.Socket, Eligible: true}
	r, err := a.State.ResolveCurrent(current)
	if err != nil {
		return thread, err
	}
	if _, duplicate := a.Currents[r.ThreadID]; duplicate {
		return thread, errors.New("fork current native ID is duplicated")
	}
	a.Currents[r.ThreadID] = current
	thread.ID = r.ThreadID
	return thread, nil
}

// The exact existing runtime and team locks cover final admission, callbacks,
// native work and the journal. No request mutex is held while waiting here.
func withProviderForkLocks(ctx context.Context, authority, stateRoot, workspace string, slugs []string, run func() error) error {
	sorted := append([]string(nil), slugs...)
	slices.Sort(sorted)
	if !slices.Equal(sorted, slugs) {
		return errors.New("fork lock scope must be sorted")
	}
	store, err := teamruntime.NewStore(stateRoot, workspace)
	if err != nil {
		return err
	}
	var releases []func()
	defer func() {
		for n := len(releases) - 1; n >= 0; n-- {
			releases[n]()
		}
	}()
	for n, slug := range slugs {
		if n > 0 && slugs[n-1] == slug {
			return errors.New("duplicate fork lock scope")
		}
		lock, err := session.LockRuntimeExclusiveContext(ctx, authority, slug)
		if err != nil {
			return err
		}
		releases = append(releases, func() { _ = lock.Close() })
	}
	for _, slug := range slugs {
		release, err := store.LockOperation(ctx, slug)
		if err != nil {
			return err
		}
		releases = append(releases, release)
	}
	return run()
}

func requireProviderForkIdle(ctx context.Context, client *workspacecodex.Client, thread providerThread) error {
	metadata, err := client.ReadThreadMetadata(ctx, thread.ID, true)
	if err != nil {
		return err
	}
	if metadata.Cwd != thread.Cwd || len(metadata.Status) != 1 || (metadata.Status["type"] != "idle" && metadata.Status["type"] != "notLoaded") {
		return errors.New("fork source has unproved native execution state")
	}
	if err := client.RequireThreadIdle(ctx, thread.ID, thread.Cwd); err != nil {
		return err
	}
	goal, err := client.HasActiveGoal(ctx, thread.ID)
	if err != nil {
		return err
	}
	if goal {
		return errors.New("fork source has an active goal")
	}
	queued, err := client.ListQueue(ctx, thread.ID)
	if err != nil {
		return err
	}
	if len(queued) != 0 {
		return errors.New("fork source has queued work")
	}
	if _, err := client.ExportSubmissionAttempts(thread.ID); err != nil {
		return err
	}
	return nil
}

func providerForkDigest(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func prepareProviderForkOrigins(state codexproviders.State, participants []providerForkSource, catalog codexproviders.Catalog) (codexproviders.State, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return state, err
	}
	candidate, err := codexproviders.DecodeBindingState(data, state.Workspace, state.Root, state.Socket, catalog)
	if err != nil {
		return state, err
	}
	changed := map[string]bool{}
	seen := map[codexproviders.BindingKey]bool{}
	leads := map[string]bool{}
	if candidate.Bindings == nil {
		candidate.Bindings = map[string]codexproviders.SessionBinding{}
	}
	for _, p := range participants {
		c, k := p.Current, p.Current.Key
		if seen[k] || !c.Eligible || c.Socket != state.Socket || c.Provider != state.Provider(k.Slug, k.RootThreadID) {
			return state, errors.New("unproved fork origin reservation")
		}
		seen[k] = true
		leads[k.Slug] = leads[k.Slug] || k.Member == "lead"
		old, exists := candidate.Bindings[k.Slug]
		if c.ExpectedRevision != old.Revision || exists && (old.RootThreadID != k.RootThreadID || old.Incarnation != k.Incarnation) {
			return state, errors.New("fork origin revision/incarnation changed")
		}
		if _, bound := old.Conversations[k.OriginalThreadID]; bound {
			if _, err := state.ResolveCurrent(c); err != nil {
				return state, err
			}
		} else {
			if c.BindingRequired || p.Thread.ID != k.OriginalThreadID {
				return state, errors.New("required fork origin binding missing")
			}
			if !exists {
				old = codexproviders.SessionBinding{RootThreadID: k.RootThreadID, Incarnation: k.Incarnation, Conversations: map[string]codexproviders.ConversationBinding{}}
			}
			old.Conversations[k.OriginalThreadID] = codexproviders.ConversationBinding{Member: k.Member, ThreadID: k.OriginalThreadID, Provider: c.Provider}
			candidate.Bindings[k.Slug] = old
			changed[k.Slug] = true
		}
	}
	if len(participants) == 0 {
		return state, errors.New("empty fork uses selection-only host path")
	}
	for _, lead := range leads {
		if !lead {
			return state, errors.New("fork origin has no lead")
		}
	}
	if len(changed) > 0 {
		if candidate.BindingRevision == math.MaxUint64 {
			return state, errors.New("fork revision exhausted")
		}
		candidate.Schema = 3
		candidate.BindingRevision++
		for slug := range changed {
			b := candidate.Bindings[slug]
			b.Revision = candidate.BindingRevision
			candidate.Bindings[slug] = b
		}
	}
	if err := codexproviders.ValidateBindings(candidate, catalog); err != nil {
		return state, err
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return state, err
	}
	if len(encoded)+1024+len(participants)*(512+36+64+128) > 1<<20 {
		return state, errors.New("intended collective fork exceeds state bounds")
	}
	return candidate, nil
}

// Capture makes no routing write. Run revalidates this frozen
// source under the same lock order before asking Ruby to reserve it.
func captureProviderFork(ctx context.Context, client *workspacecodex.Client, path, stateRoot, authority, profile, sourcePackage string, previous []byte, catalog codexproviders.Catalog, required map[string]bool, environment func(codexproviders.CurrentBinding) (map[string]string, error), verify func(context.Context, session.RuntimeAuthority) error) error {
	if environment == nil || verify == nil {
		return errors.New("fork owning environment/runtime proof unavailable")
	}
	var state codexproviders.State
	if err := json.Unmarshal(previous, &state); err != nil {
		return err
	}
	var err error
	state, err = codexproviders.DecodeBindingState(previous, state.Workspace, state.Root, state.Socket, catalog)
	if err != nil {
		return err
	}
	if state.Operation == nil || state.Operation.Strategy != "fork-v1" || state.Operation.Phase != "waiting" || len(state.Operation.Sessions) == 0 || state.Operation.CatalogDigest != catalog.Digest || !filepath.IsAbs(sourcePackage) {
		return errors.New("fork capture requires an exact pending operation")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("fork source snapshot already exists or is unavailable")
	}
	return withProviderForkLocks(ctx, authority, stateRoot, state.Root, state.Operation.Sessions, func() error {
		admission := providerForkAdmission{State: state, Authority: authority, Required: required, Currents: map[string]codexproviders.CurrentBinding{}, Rosters: map[string]providerForkRoster{}}
		scope := map[string]bool{}
		for _, slug := range state.Operation.Sessions {
			scope[slug] = true
		}
		inventory, err := buildProviderInventoryAdmission(ctx, client, state.Root, stateRoot, state.Socket, profile, scope, &admission)
		if err != nil {
			return err
		}
		if len(inventory.Blockers) != 0 {
			return errors.New("fork scope has unresolved monitor work")
		}
		identity, err := session.SocketIdentity(state.Socket)
		if err != nil {
			return err
		}
		selection := codexproviders.SelectionChange{Scope: "workspace", Provider: state.Operation.Target}
		if state.Operation.Session != "" {
			selection.Scope = "session"
			selection.Session = state.Operation.Session
			selection.Inherit = state.Operation.Inherit
		}
		source := providerForkSnapshot{Schema: 1, Strategy: "fork-v1", OperationID: state.Operation.ID, Workspace: state.Workspace, Root: state.Root, Socket: state.Socket, CatalogDigest: catalog.Digest, SourcePackage: sourcePackage, PreviousStateDigest: providerForkDigest(previous), Selection: selection, Sessions: state.Operation.Sessions, SocketIdentity: identity, Rosters: admission.Rosters, Participants: []providerForkSource{}}
		changing := false
		for _, current := range admission.Currents {
			changing = changing || current.Provider != state.Operation.Target
		}
		if !changing {
			return errors.New("unchanged provider uses the selection-only writer")
		}
		for _, thread := range inventory.Threads {
			current := admission.Currents[thread.ID]
			env, err := environment(current)
			if err != nil {
				return errors.New("fork participant environment is unsupported")
			}
			thread.Environment = map[string]string{}
			for k, v := range env {
				thread.Environment[k] = v
			}
			if err := validateProviderForkEnvironment(thread, current, state); err != nil {
				return err
			}
			participant := providerForkSource{Current: current, Thread: thread}
			if err := captureProviderForkThread(ctx, client, &participant, current.Provider); err != nil {
				return err
			}
			records, err := web.FreezeProviderForkBrowserRecords(stateRoot, state.Root, current.Key.Slug, current.Key.RootThreadID, thread.ID)
			if err != nil {
				return err
			}
			participant.BrowserRecords = &records
			if err := validateProviderResumePayloads(participant.Thread, state.Operation.Target); err != nil {
				return err
			}
			source.Participants = append(source.Participants, participant)
		}
		candidate, err := prepareProviderForkOrigins(state, source.Participants, catalog)
		if err != nil {
			return err
		}
		operation := *candidate.Operation
		operation.Phase = "applying"
		operation.Stage = "preparing"
		operation.Snapshot = path
		candidate.Operation = &operation
		source.State = candidate
		if err := validateProviderForkSnapshot(source, path, catalog); err != nil {
			return err
		}
		if err := recheckProviderForkScope(ctx, client, source, stateRoot, authority, verify, source.SocketIdentity); err != nil {
			return err
		}
		return providerAtomicJSON(path, source)
	})
}

func validateProviderForkEnvironment(thread providerThread, current codexproviders.CurrentBinding, state codexproviders.State) error {
	env := thread.Environment
	k := current.Key
	if env["DEV_SESSION_SLUG"] != k.Slug || env["DEV_SESSION_WORKSPACE"] != state.Root || env["DEV_SESSION_WORK_DIR"] != thread.Cwd || env["DEV_SESSION_WORKTREES_DIR"] != filepath.Join(state.Root, "worktrees", k.Slug) || env["DEV_SESSION_CODEX_SOCKET"] != state.Socket || env["DEV_SESSION_REQUIRE_RUNTIME"] != "1" || env["DEV_SESSION_MEMBER_ADDRESS"] != thread.MemberAddress {
		return errors.New("fork participant environment identity changed")
	}
	for _, key := range []string{"DEV_SESSION_PORTAL_BASE_URL", "DEV_SESSION_URL", "DEV_SESSION_AUTHORITY_DIR", "DEV_SESSION_TMUX_SOCKET", "DEV_SESSION_CODEX", "DEV_SESSION_CODEX_VERSION", "DEV_SESSION_PORTAL_COMMAND", "DEV_WORKSPACE_CODEX_HOME"} {
		if env[key] == "" {
			return errors.New("fork participant environment is incomplete")
		}
	}
	return nil
}

func captureProviderForkThread(ctx context.Context, client *workspacecodex.Client, p *providerForkSource, provider string) error {
	var metadata struct {
		Thread map[string]any `json:"thread"`
	}
	if err := client.Request(ctx, "thread/read", map[string]any{"threadId": p.Thread.ID, "excludeTurns": true}, &metadata); err != nil {
		return err
	}
	m := metadata.Thread
	if m == nil || m["id"] != p.Thread.ID || m["cwd"] != p.Thread.Cwd || m["modelProvider"] != provider || m["historyMode"] != "paginated" {
		return errors.New("fork source metadata is incomplete")
	}
	name, ok := m["name"].(string)
	if m["name"] != nil && !ok {
		return errors.New("fork source title is invalid")
	}
	p.Title = name
	params := map[string]any{"threadId": p.Thread.ID, "excludeTurns": true}
	status, _ := m["status"].(map[string]any)
	if status["type"] == "notLoaded" {
		path, ok := m["path"].(string)
		if !ok {
			return errors.New("cold fork source lacks retained policy")
		}
		if err := preserveColdNativePolicy(path, params); err != nil {
			return err
		}
	}
	var original map[string]any
	if err := client.Request(ctx, "thread/resume", params, &original); err != nil {
		return err
	}
	p.Thread.Original = original
	if err := validateProviderResponse(p.Thread, p.Thread.Original, provider); err != nil {
		return err
	}
	for _, key := range []string{"approvalPolicy", "approvalsReviewer", "sandbox", "runtimeWorkspaceRoots"} {
		if p.Thread.Original[key] == nil {
			return errors.New("fork effective policy is incomplete")
		}
	}
	if !supportedProviderPermissionProfile(p.Thread.Original["activePermissionProfile"]) {
		return errors.New("fork permission profile is unsupported")
	}
	history, err := readProviderForkHistory(ctx, client, p.Thread.ID)
	if err != nil {
		return err
	}
	p.History = history
	return nil
}

func validateProviderForkSnapshot(source providerForkSnapshot, path string, catalog codexproviders.Catalog) error {
	state := source.State
	o := state.Operation
	if source.Schema != 1 || source.Strategy != "fork-v1" || o == nil || source.OperationID != o.ID || path != o.Snapshot || source.Workspace != state.Workspace || source.Root != state.Root || source.Socket != state.Socket || source.CatalogDigest != o.CatalogDigest || source.CatalogDigest != catalog.Digest || !slices.Equal(source.Sessions, o.Sessions) || len(source.Rosters) != len(source.Sessions) || !canonicalAbsolutePath(source.SourcePackage) || len(source.PreviousStateDigest) != 64 || len(source.Participants) == 0 {
		return errors.New("fork source ownership changed")
	}
	if err := codexproviders.ValidateBindings(state, catalog); err != nil {
		return err
	}
	expected := codexproviders.SelectionChange{Scope: "workspace", Provider: o.Target}
	if o.Session != "" {
		expected.Scope = "session"
		expected.Session = o.Session
		expected.Inherit = o.Inherit
	}
	if expected != source.Selection {
		return errors.New("fork frozen selection changed")
	}
	if expected.Inherit && expected.Provider != state.Active {
		return errors.New("fork inherited target differs from workspace default")
	}
	for _, slug := range source.Sessions {
		if source.Rosters[slug].BindingOriginSchema < 1 || source.Rosters[slug].BindingOriginSchema > 3 {
			return errors.New("fork frozen manifest schema is missing")
		}
		if expected.Scope == "workspace" && state.Sessions[slug].RootThreadID == o.Roots[slug] {
			return errors.New("explicit session is outside inherited fork scope")
		}
	}

	seen := map[codexproviders.BindingKey]bool{}
	for _, p := range source.Participants {
		k := p.Current.Key
		binding, ok := state.Bindings[k.Slug]
		if !ok || seen[k] || p.Thread.Slug != k.Slug || p.Thread.Cwd != filepath.Join(state.Root, "work", k.Slug) || p.Thread.MemberAddress != "" && p.Thread.MemberAddress != k.Member || p.Thread.MemberAddress == "" && k.Member != "lead" {
			return errors.New("fork frozen participant identity changed")
		}
		seen[k] = true
		current := p.Current
		current.ExpectedRevision = binding.Revision
		current.BindingRequired = true
		resolved, err := state.ResolveCurrent(current)
		if err != nil || resolved.ThreadID != p.Thread.ID {
			return errors.New("fork frozen active source changed")
		}
		if err := validateProviderForkEnvironment(p.Thread, p.Current, state); err != nil {
			return err
		}
		if err := validateProviderResponse(p.Thread, p.Thread.Original, p.Current.Provider); err != nil {
			return err
		}
		if p.History.Turns == nil || p.History.Items == nil || p.BrowserRecords == nil {
			return errors.New("fork source history is absent")
		}
	}
	return nil
}

func validateProviderForkReservation(state codexproviders.State, source providerForkSnapshot, journal codexproviders.ForkJournal, catalog codexproviders.Catalog) error {
	if err := codexproviders.ValidateBindings(state, catalog); err != nil {
		return err
	}
	want, got := source.State.Operation, state.Operation
	if got == nil || want == nil {
		return errors.New("fork reservation is absent")
	}
	a, b := *want, *got
	a.Phase, a.Stage, a.UpdatedAt, a.Message, a.Blockers = "", "", "", "", nil
	b.Phase, b.Stage, b.UpdatedAt, b.Message, b.Blockers = "", "", "", "", nil
	if !reflect.DeepEqual(a, b) || !slices.Contains([]string{"applying", "recovery-required"}, got.Phase) || state.Active != source.State.Active {
		return errors.New("fork reservation operation changed")
	}
	for _, slug := range source.Sessions {
		if !reflect.DeepEqual(state.Bindings[slug], source.State.Bindings[slug]) || !reflect.DeepEqual(state.Sessions[slug], source.State.Sessions[slug]) {
			return errors.New("fork reservation binding/selection changed")
		}
	}
	for _, p := range journal.Participants {
		if _, err := state.ResolveCurrent(p.Current); err != nil {
			return err
		}
	}
	return nil
}

func writeProviderForkJournal(path string, journal codexproviders.ForkJournal, source providerForkSnapshot, catalog codexproviders.Catalog) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if _, err := codexproviders.DecodeForkJournal(data, source.State, catalog); err != nil {
		return err
	}
	return providerAtomicJSON(path, journal)
}

type providerForkCallbacks struct {
	Reserve     func(context.Context, string, string) error
	FirstIntent func(context.Context, string, string) error
	Commit      func(context.Context, string, string) error
	ReadState   func() (codexproviders.State, error)
	// R2-C supplies current runtime/terminal generation proof under these locks.
	// It is required, not a permissive default or a browser-provided assertion.
	VerifyAuthority func(context.Context, session.RuntimeAuthority) error
}

// The exact frozen package owns the only two state-writer actions. A failed
// acknowledgement is an error, never permission to retry a native operation.
func providerForkHostCallbacks(source providerForkSnapshot, stateRoot, profile string, catalog codexproviders.Catalog) (providerForkCallbacks, error) {
	selected, err := filepath.EvalSymlinks(profile)
	if err != nil || selected != source.SourcePackage {
		return providerForkCallbacks{}, errors.New("fork source package selection changed")
	}
	var contract map[string]json.RawMessage
	data, err := os.ReadFile(filepath.Join(selected, "share/workspace-portal/runtime-contract.json"))
	if err != nil || session.DecodePiCreationJSON(data, &contract, 64<<10) != nil || string(contract["codexProviderBindingStateSchema"]) != "3" || string(contract["codexProviderForkTransitionPolicy"]) != "1" || string(contract["codexRuntimeAuthoritySchema"]) != "3" {
		return providerForkCallbacks{}, errors.New("fork binding consumers are unavailable")
	}
	call := func(ctx context.Context, action, step, path, digest string) error {
		cmd := exec.CommandContext(ctx, filepath.Join(selected, "bin/workspace-host"), "provider", action, source.Workspace, source.OperationID, path, digest, step)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.WaitDelay = 2 * time.Second
		cmd.Cancel = func() error {
			err := unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
			if errors.Is(err, unix.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		output, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("fork writer %s acknowledgement failed: %w", step, err)
		}
		var ack struct {
			Schema      int    `json:"schema"`
			OperationID string `json:"operationId"`
			Step        string `json:"step"`
			ProofDigest string `json:"proofDigest"`
		}
		decoder := json.NewDecoder(bytes.NewReader(output))
		decoder.DisallowUnknownFields()
		if len(output) > 4096 || decoder.Decode(&ack) != nil || decoder.Decode(new(any)) != io.EOF || ack.Schema != 1 || ack.OperationID != source.OperationID || ack.Step != step || ack.ProofDigest != digest {
			return errors.New("fork writer acknowledgement is invalid")
		}
		return nil
	}
	callbacks := providerForkCallbacks{
		Reserve: func(ctx context.Context, path, digest string) error {
			return call(ctx, "fork-reserve", "reserve", path, digest)
		},
		FirstIntent: func(ctx context.Context, path, digest string) error {
			return call(ctx, "fork-reserve", "first-intent", path, digest)
		},
		Commit: func(ctx context.Context, path, digest string) error {
			return call(ctx, "fork-commit", "commit", path, digest)
		},
		ReadState: func() (codexproviders.State, error) {
			path, err := codexproviders.StatePath(stateRoot, source.Workspace)
			if err != nil {
				return codexproviders.State{}, err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return codexproviders.State{}, err
			}
			return codexproviders.DecodeBindingState(data, source.Workspace, source.Root, source.Socket, catalog)
		},
		VerifyAuthority: func(ctx context.Context, a session.RuntimeAuthority) error { return a.VerifyTmux(ctx, "") },
	}
	return callbacks, nil
}

func providerForkCommand(ctx context.Context, client *workspacecodex.Client, path, stateRoot, authority, profile, workspace, socket, operation, target string, catalog codexproviders.Catalog) error {
	selected, err := filepath.EvalSymlinks(profile)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil || filepath.Dir(filepath.Dir(executable)) != selected {
		return errors.New("fork executor is not the current selected package")
	}
	if _, err := providerForkHostCallbacks(providerForkSnapshot{SourcePackage: selected}, stateRoot, profile, catalog); err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		name := filepath.Base(filepath.Dir(socket))
		statePath, err := codexproviders.StatePath(stateRoot, name)
		if err != nil || path != filepath.Join(filepath.Dir(statePath), operation+".snapshot.json") {
			return errors.New("fork capture path changed")
		}
		var state codexproviders.State
		previous, err := readProviderForkJSON(statePath, &state)
		if err != nil {
			return err
		}
		state, err = codexproviders.DecodeBindingState(previous, name, workspace, socket, catalog)
		if err != nil || state.Operation == nil || state.Operation.ID != operation || state.Operation.Target != target || state.Operation.Strategy != "fork-v1" || state.Operation.Phase != "waiting" || state.Operation.Snapshot != path {
			return errors.New("fork capture requires its original waiting operation")
		}
		for _, retained := range []string{path + ".journal", path + ".verified"} {
			if _, err := os.Lstat(retained); !errors.Is(err, os.ErrNotExist) {
				return errors.New("missing fork snapshot has retained operation evidence")
			}
		}
		selected, err := filepath.EvalSymlinks(profile)
		if err != nil {
			return err
		}
		required := map[string]bool{}
		for _, slug := range state.Operation.Sessions {
			summary, err := session.Find(workspace, slug)
			if err != nil || summary.Archived || summary.Codex.ThreadID != state.Operation.Roots[slug] {
				return errors.New("fork accepted session changed before capture")
			}
			required[slug] = summary.Codex.BindingSchema == 3
		}
		environment := func(current codexproviders.CurrentBinding) (map[string]string, error) {
			values := map[string]string{}
			for _, key := range []string{"DEV_SESSION_PORTAL_BASE_URL", "DEV_SESSION_AUTHORITY_DIR", "DEV_SESSION_TMUX_SOCKET", "DEV_SESSION_CODEX", "DEV_SESSION_CODEX_VERSION", "DEV_SESSION_PORTAL_COMMAND", "DEV_WORKSPACE_CODEX_HOME"} {
				values[key] = os.Getenv(key)
			}
			slug := current.Key.Slug
			values["DEV_SESSION_SLUG"], values["DEV_SESSION_WORKSPACE"] = slug, workspace
			values["DEV_SESSION_WORK_DIR"] = filepath.Join(workspace, "work", slug)
			values["DEV_SESSION_WORKTREES_DIR"] = filepath.Join(workspace, "worktrees", slug)
			values["DEV_SESSION_URL"] = values["DEV_SESSION_PORTAL_BASE_URL"] + "/" + slug + "/"
			values["DEV_SESSION_CODEX_SOCKET"], values["DEV_SESSION_REQUIRE_RUNTIME"] = socket, "1"
			if current.Key.Member != "lead" {
				values["DEV_SESSION_MEMBER_ADDRESS"] = current.Key.Member
			}
			return values, nil
		}
		if err := captureProviderFork(ctx, client, path, stateRoot, authority, profile, selected, previous, catalog, required, environment, func(ctx context.Context, a session.RuntimeAuthority) error { return a.VerifyTmux(ctx, "") }); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	var source providerForkSnapshot
	if _, err := readProviderForkJSON(path, &source); err != nil {
		return err
	}
	if source.Root != workspace || source.Socket != socket || source.OperationID != operation || source.State.Operation == nil || source.State.Operation.Target != target {
		return errors.New("fork helper ownership changed")
	}
	callbacks, err := providerForkHostCallbacks(source, stateRoot, profile, catalog)
	if err != nil {
		return err
	}
	firstIntent := callbacks.FirstIntent
	callbacks.FirstIntent = func(ctx context.Context, path, digest string) error {
		models, err := codexproviders.ReadModels(ctx, profile, workspace, source.Workspace, target)
		if err != nil {
			return err
		}
		for _, participant := range source.Participants {
			if _, err := workspacecodex.ResolveNewThreadSettings(models, participant.Thread.Settings); err != nil {
				return err
			}
		}
		return firstIntent(ctx, path, digest)
	}
	if err := runProviderFork(ctx, client, path, stateRoot, authority, catalog, callbacks); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": 1, "operationId": operation, "verified": true})
}

func readProviderForkJSON(path string, out any) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16<<20 {
		return nil, errors.New("fork record is not private and bounded")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("fork record has trailing JSON")
	}
	return data, nil
}

func readProviderForkHistory(ctx context.Context, client *workspacecodex.Client, thread string) (providerForkHistory, error) {
	result := providerForkHistory{Turns: []json.RawMessage{}, Items: []json.RawMessage{}}
	bytesRead := 0
	for _, method := range []string{"thread/turns/list", "thread/items/list"} {
		cursor := ""
		seen := map[string]bool{}
		for {
			params := map[string]any{"threadId": thread, "limit": 100, "sortDirection": "asc"}
			if method == "thread/turns/list" {
				params["itemsView"] = "full"
			}
			if cursor != "" {
				params["cursor"] = cursor
			}
			var page struct {
				Data *[]json.RawMessage `json:"data"`
				Next *string            `json:"nextCursor"`
			}
			if err := client.Request(ctx, method, params, &page); err != nil {
				return result, err
			}
			if page.Data == nil {
				return result, errors.New("fork history page is absent")
			}
			for _, raw := range *page.Data {
				bytesRead += len(raw)
				if bytesRead > 16<<20 {
					return result, errors.New("fork history exceeds snapshot bounds")
				}
				var value any
				d := json.NewDecoder(bytes.NewReader(raw))
				d.UseNumber()
				if err := d.Decode(&value); err != nil {
					return result, err
				}
				canonical, err := json.Marshal(value)
				if err != nil {
					return result, err
				}
				if method == "thread/turns/list" {
					result.Turns = append(result.Turns, canonical)
				} else {
					result.Items = append(result.Items, canonical)
				}
			}
			if page.Next == nil {
				break
			}
			if *page.Next == "" || seen[*page.Next] || len(*page.Data) == 0 {
				return result, errors.New("fork history cursor is empty or repeated")
			}
			seen[*page.Next] = true
			cursor = *page.Next
		}
	}
	return result, nil
}

func recheckProviderForkScope(ctx context.Context, client *workspacecodex.Client, source providerForkSnapshot, stateRoot, authority string, verify func(context.Context, session.RuntimeAuthority) error, expectedEpoch string) error {
	if verify == nil {
		return errors.New("fork current runtime proof is unavailable")
	}
	identity, err := session.SocketIdentity(source.Socket)
	if err != nil || identity != expectedEpoch {
		return errors.New("fork native socket generation changed")
	}
	store, err := teamruntime.NewStore(stateRoot, source.Root)
	if err != nil {
		return err
	}
	for _, slug := range source.Sessions {
		frozen, ok := source.Rosters[slug]
		if !ok {
			return errors.New("fork frozen roster is missing")
		}
		summary, err := session.Find(source.Root, slug)
		if err != nil || summary.Archived || summary.Codex.ThreadID != frozen.Root || summary.Codex.SocketPath != source.Socket {
			return errors.New("fork session origin changed")
		}
		origin := summary.Schema
		if origin == 6 {
			origin = summary.Codex.BindingOriginSchema
		}
		if origin != frozen.BindingOriginSchema {
			return errors.New("fork original manifest schema changed")
		}
		if owner, err := session.PendingLifecycle(source.Root, slug); err != nil || owner != "" {
			return errors.New("fork session lifecycle is unfinished")
		}
		a, err := session.LoadRuntimeAuthority(authority, slug, source.Root)
		if err != nil || a.TmuxIdentity != frozen.Incarnation || a.CodexThreadID != frozen.Root || a.CodexSocketPath != source.Socket {
			return errors.New("fork runtime incarnation changed")
		}
		if err := verify(ctx, a); err != nil {
			return err
		}
		roster, err := store.Load(slug, frozen.Root)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		members := map[string]string{frozen.Root: "lead"}
		if roster == nil {
			if frozen.Revision != 0 {
				return errors.New("fork roster disappeared")
			}
		} else {
			if roster.Revision != frozen.Revision {
				return errors.New("fork roster revision changed")
			}
			for _, m := range roster.Members {
				if m.State == "removed" || m.State == "archived" {
					continue
				}
				if m.State != "ready" || m.RetireIntent != "" || m.Thread == "" {
					return errors.New("fork member transition is unfinished")
				}
				members[m.Thread] = m.Address
			}
		}
		count := 0
		for _, p := range source.Participants {
			if p.Current.Key.Slug != slug {
				continue
			}
			count++
			k := p.Current.Key
			if members[k.OriginalThreadID] != k.Member || k.RootThreadID != frozen.Root || k.Incarnation != frozen.Incarnation {
				return errors.New("fork participant is not current roster identity")
			}
			if err := requireProviderForkIdle(ctx, client, p.Thread); err != nil {
				return err
			}
			records, err := web.FreezeProviderForkBrowserRecords(stateRoot, source.Root, slug, frozen.Root, p.Thread.ID)
			if err != nil || p.BrowserRecords == nil || !reflect.DeepEqual(records, *p.BrowserRecords) {
				return errors.New("fork frozen browser records changed")
			}
			observed := p
			if err := captureProviderForkThread(ctx, client, &observed, p.Current.Provider); err != nil {
				return err
			}
			if observed.Title != p.Title || !reflect.DeepEqual(observed.History, p.History) {
				return errors.New("fork source history/title changed")
			}
			left, right := map[string]any{}, map[string]any{}
			for _, key := range []string{"model", "modelProvider", "reasoningEffort", "cwd", "approvalPolicy", "approvalsReviewer", "sandbox", "runtimeWorkspaceRoots", "activePermissionProfile", "serviceTier", "disabledPluginIds", "collaborationMode", "multiAgentMode"} {
				left[key] = p.Thread.Original[key]
				right[key] = observed.Thread.Original[key]
			}
			if !reflect.DeepEqual(left, right) {
				return errors.New("fork source effective settings changed")
			}

			retry, err := (turnretry.Store{Root: stateRoot, Workspace: source.Root}).Load(turnretry.Scope{Workspace: source.Root, Slug: slug, Root: frozen.Root, Thread: p.Thread.ID, Socket: source.Socket})
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil && retry.Owned() {
				return errors.New("fork source has unresolved retry work")
			}
		}
		if count != len(members) {
			return errors.New("fork frozen roster is incomplete")
		}
		recovery, err := (session.RecoveryStore{Root: stateRoot, Workspace: source.Root}).Load(slug)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && (recovery.ThreadID != frozen.Root || recovery.SocketPath != source.Socket || recovery.Continuation != nil) {
			return errors.New("fork session recovery is unresolved")
		}
	}
	if err := recheckProviderForkTemporaryWork(ctx, client, source, stateRoot); err != nil {
		return err
	}
	return requireProviderForkEpoch(source.Socket, expectedEpoch)
}

func requireProviderForkEpoch(socket, expected string) error {
	epoch, err := session.SocketIdentity(socket)
	if err != nil || expected == "" || epoch != expected {
		return errors.New("fork native socket generation changed")
	}
	return nil
}

func recheckProviderForkTemporaryWork(ctx context.Context, client *workspacecodex.Client, source providerForkSnapshot, stateRoot string) error {
	store, err := monitoroperation.NewStore(stateRoot, source.Root)
	if err != nil {
		return err
	}
	for _, slug := range source.Sessions {
		inventory, err := store.Inventory(ctx, slug, source.Rosters[slug].Root)
		if err != nil {
			return err
		}
		if len(inventory.Unresolved) > 0 || len(inventory.Blockers) > 0 {
			return errors.New("fork scope has unresolved utility work")
		}
		for _, thread := range inventory.Threads {
			if err := requireProviderForkIdle(ctx, client, providerThread{ID: thread.ThreadID, Cwd: thread.Cwd}); err != nil {
				return err
			}
		}
	}
	ids, err := client.LoadedThreadIDs(ctx)
	if err != nil {
		return err
	}
	parents := map[string]string{}
	metadata := map[string]codex.ThreadMetadata{}
	affected := map[string]bool{}
	cwds := map[string]bool{}
	for _, p := range source.Participants {
		affected[p.Thread.ID] = true
		cwds[p.Thread.Cwd] = true
	}
	for _, id := range ids {
		if affected[id] {
			continue
		}
		m, err := client.ReadThreadMetadata(ctx, id, true)
		if err != nil {
			return err
		}
		if m.ID != id {
			return errors.New("fork descendant identity changed")
		}
		if !cwds[m.Cwd] {
			continue
		}
		metadata[id] = m
		if raw, ok := m.Source.(map[string]any); ok {
			sub, _ := raw["subAgent"].(map[string]any)
			spawn, _ := sub["thread_spawn"].(map[string]any)
			parent, _ := spawn["parent_thread_id"].(string)
			if parent == "" {
				return errors.New("fork affected-tree relationship is unproved")
			}
			parents[id] = parent
		} else if m.Source != "vscode" {
			return errors.New("fork affected-tree relationship is unavailable")
		}
	}
	for id, m := range metadata {
		seen := map[string]bool{}
		parent := parents[id]
		for parent != "" && !affected[parent] && !seen[parent] {
			seen[parent] = true
			parent = parents[parent]
		}
		if affected[parent] {
			if err := requireProviderForkIdle(ctx, client, providerThread{ID: id, Cwd: m.Cwd}); err != nil {
				return err
			}
		} else if parents[id] != "" {
			return errors.New("fork descendant ancestry is incomplete")
		}
	}
	return nil
}

func verifyProviderForkChild(ctx context.Context, client *workspacecodex.Client, p providerForkSource, child, target string, effective map[string]any, socket, expectedEpoch, savedVerification string) (string, error) {
	if err := requireProviderForkEpoch(socket, expectedEpoch); err != nil {
		return "", err
	}
	copy := p.Thread
	copy.ID = child
	if err := requireProviderForkIdle(ctx, client, copy); err != nil {
		return "", err
	}
	if err := validateProviderResponse(copy, effective, target); err != nil {
		return "", err
	}
	native, _ := effective["thread"].(map[string]any)
	if child == p.Thread.ID || native["forkedFromId"] != p.Thread.ID {
		return "", errors.New("fork returned wrong parent or reused source")
	}
	for _, key := range []string{"runtimeWorkspaceRoots", "approvalPolicy", "approvalsReviewer", "sandbox", "activePermissionProfile", "serviceTier", "disabledPluginIds", "collaborationMode", "multiAgentMode"} {
		if key == "serviceTier" && providerTierMatches(p.Thread.Original[key], effective[key]) {
			continue
		}
		if !reflect.DeepEqual(p.Thread.Original[key], effective[key]) {
			return "", fmt.Errorf("fork changed retained %s", key)
		}
	}
	var metadata struct {
		Thread map[string]any `json:"thread"`
	}
	if err := client.Request(ctx, "thread/read", map[string]any{"threadId": child, "excludeTurns": true}, &metadata); err != nil {
		return "", err
	}
	m := metadata.Thread
	if m == nil || m["id"] != child || m["cwd"] != p.Thread.Cwd || m["modelProvider"] != target || m["forkedFromId"] != p.Thread.ID || m["historyMode"] != "paginated" {
		return "", errors.New("known fork child metadata changed")
	}
	if m["name"] != p.Title {
		if p.Title == "" || savedVerification != "" {
			return "", errors.New("fork changed a retained verified title")
		}
		if err := requireProviderForkEpoch(socket, expectedEpoch); err != nil {
			return "", err
		}
		if err := client.SetName(ctx, child, p.Title); err != nil {
			return "", err
		}
		var renamed struct {
			Thread map[string]any `json:"thread"`
		}
		if err := client.Request(ctx, "thread/read", map[string]any{"threadId": child, "excludeTurns": true}, &renamed); err != nil {
			return "", err
		}
		m = renamed.Thread
		if m == nil || m["name"] != p.Title || m["id"] != child || m["cwd"] != p.Thread.Cwd || m["modelProvider"] != target || m["forkedFromId"] != p.Thread.ID || m["historyMode"] != "paginated" {
			return "", errors.New("fork title or metadata was not preserved")
		}

	}
	history, err := readProviderForkHistory(ctx, client, child)
	if err != nil {
		return "", err
	}
	if !reflect.DeepEqual(history, p.History) {
		return "", errors.New("fork inherited history differs from frozen source")
	}
	if err := requireProviderForkEpoch(socket, expectedEpoch); err != nil {
		return "", err
	}
	settings, _ := json.Marshal(effective)
	hist, _ := json.Marshal(history)
	title, _ := json.Marshal(p.Title)
	proof, _ := json.Marshal(map[string]any{"key": p.Current.Key, "sourceThreadId": p.Thread.ID, "childThreadId": child, "provider": target, "historyDigest": providerForkDigest(hist), "settingsDigest": providerForkDigest(settings), "titleDigest": providerForkDigest(title)})
	return string(proof), nil
}

// A fresh or original-operation forward recovery. Callbacks are synchronous and
// operation/digest-bound; the private command retains its selected package owner.
func runProviderFork(ctx context.Context, client *workspacecodex.Client, path, stateRoot, authority string, catalog codexproviders.Catalog, callbacks providerForkCallbacks) error {
	var source providerForkSnapshot
	sourceBytes, err := readProviderForkJSON(path, &source)
	if err != nil {
		return err
	}
	if err := validateProviderForkSnapshot(source, path, catalog); err != nil {
		return err
	}
	if callbacks.Reserve == nil || callbacks.FirstIntent == nil || callbacks.Commit == nil || callbacks.ReadState == nil {
		return errors.New("fork writer callbacks unavailable")
	}
	digest := providerForkDigest(sourceBytes)
	attemptEpoch := source.SocketIdentity
	return withProviderForkLocks(ctx, authority, stateRoot, source.Root, source.Sessions, func() error {
		journalPath := path + ".journal"
		load := func() (codexproviders.ForkJournal, error) {
			data, err := os.ReadFile(journalPath)
			if err != nil {
				return codexproviders.ForkJournal{}, err
			}
			j, err := codexproviders.DecodeForkJournal(data, source.State, catalog)
			if err == nil && (j.SourceSnapshotDigest != digest || j.SourcePackage != source.SourcePackage) {
				err = errors.New("fork journal source proof changed")
			}
			return j, err
		}
		if _, err := os.Lstat(journalPath); errors.Is(err, os.ErrNotExist) {
			if err := recheckProviderForkScope(ctx, client, source, stateRoot, authority, callbacks.VerifyAuthority, attemptEpoch); err != nil {
				return err
			}
			if err := callbacks.Reserve(ctx, path, digest); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		journal, err := load()
		if err != nil {
			return err
		}
		if !slices.EqualFunc(journal.Participants, source.Participants, func(p codexproviders.ForkParticipant, s providerForkSource) bool {
			return p.Current.Key == s.Current.Key && p.SourceThreadID == s.Thread.ID
		}) {
			return errors.New("fork journal participant set changed")
		}
		state, err := callbacks.ReadState()
		if err != nil {
			return err
		}
		// A state rename may have committed before the journal acknowledgement.
		// Only the original writer may reconcile that sealed operation; never fork.
		if journal.Stage == "restoring-terminals" || state.Operation != nil && state.Operation.ID == source.OperationID && state.Operation.Stage == "restoring-terminals" {
			epoch, err := session.SocketIdentity(source.Socket)
			if err != nil {
				return err
			}
			readCommitted := func() (codexproviders.State, codexproviders.ForkJournal, error) {
				state, err := callbacks.ReadState()
				if err != nil {
					return state, codexproviders.ForkJournal{}, err
				}
				journal, err := load()
				if err != nil {
					return state, journal, err
				}
				view := journal
				if view.Stage == "committing" {
					// A read view only; sealed current bindings must prove the
					// rename before the original writer repairs durable progress.
					view.Stage = "restoring-terminals"
				}
				return state, journal, verifyProviderForkCommit(state, view, source, path, catalog)
			}
			state, saved, err := readCommitted()
			if err != nil {
				return err
			}
			if err := callbacks.Reserve(ctx, path, digest); err != nil {
				return err
			}
			state, journal, err = readCommitted()
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(saved, journal) {
				return errors.New("fork committed admission changed saved progress")
			}
			if err := requireProviderForkEpoch(source.Socket, epoch); err != nil {
				return err
			}
			for n, p := range journal.Participants {
				var proof string
				if _, err := readProviderForkJSON(fmt.Sprintf("%s.proof-%d", path, n), &proof); err != nil {
					return err
				}
				if p.State != "verified" || providerForkDigest([]byte(proof)) != p.VerificationDigest {
					return errors.New("fork committed verification changed")
				}
			}
			if state.Operation.Phase == "applied" {
				for _, p := range journal.Projections {
					if !p.Manifest || !p.Authority || !p.Recovery || !p.Terminal {
						return errors.New("completed fork has incomplete projections")
					}
				}
				return nil // Later ordinary history is not this completed operation.
			}
			for n, p := range journal.Participants {
				if err := requireProviderForkEpoch(source.Socket, epoch); err != nil {
					return err
				}
				var effective map[string]any
				if err := client.Request(ctx, "thread/resume", map[string]any{"threadId": p.ChildThreadID, "excludeTurns": true}, &effective); err != nil {
					return err
				}
				if _, err := verifyProviderForkChild(ctx, client, source.Participants[n], p.ChildThreadID, source.State.Operation.Target, effective, source.Socket, epoch, p.VerificationDigest); err != nil {
					return err
				}
			}
			if _, current, err := readCommitted(); err != nil {
				return err
			} else if !reflect.DeepEqual(saved, current) {
				return errors.New("fork committed proof changed during admission")
			}
			attemptEpoch = epoch
			return finishProviderForkProjections(ctx, source, path, stateRoot, catalog, callbacks, load, attemptEpoch)
		}
		if state.Operation != nil && state.Operation.Phase == "waiting" && journal.Stage == "preparing" {
			if err := callbacks.Reserve(ctx, path, digest); err != nil {
				return err
			}
			state, err = callbacks.ReadState()
			if err != nil {
				return err
			}
		}
		if err := validateProviderForkReservation(state, source, journal, catalog); err != nil {
			return err
		}
		// An explicitly resumed pre-intent operation may need to finish the
		// acknowledged origin projections. Reconcile the exact reservation first;
		// this idempotent reserve action never authorizes a native request.
		incompleteOrigins := false
		for _, projection := range journal.Projections {
			incompleteOrigins = incompleteOrigins || !projection.Manifest || !projection.Authority
		}
		if journal.Stage == "preparing" && incompleteOrigins {
			for _, participant := range journal.Participants {
				if participant.State != "not-attempted" || participant.ChildThreadID != "" || participant.VerificationDigest != "" {
					return errors.New("fork intent cannot reconcile origin projections")
				}
			}
			if err := callbacks.Reserve(ctx, path, digest); err != nil {
				return err
			}
			journal, err = load()
			if err != nil {
				return err
			}
			state, err = callbacks.ReadState()
			if err != nil {
				return err
			}
			if err := validateProviderForkReservation(state, source, journal, catalog); err != nil {
				return err
			}
		}
		if journal.Stage == "preparing" {
			for _, projection := range journal.Projections {
				if !projection.Manifest || !projection.Authority {
					return errors.New("fork origin projections remain incomplete")
				}
			}
		}
		for _, p := range journal.Participants {
			if p.State == "intent" {
				return errors.New("fork intent outcome is unknown; no native replay")
			}
		}
		// Capture provenance stays immutable. Only the acknowledged original
		// recovery may establish one endpoint epoch for this locked attempt.
		known := slices.ContainsFunc(journal.Participants, func(p codexproviders.ForkParticipant) bool {
			return p.State == "known-child" || p.State == "verified"
		})
		if known {
			epoch, err := session.SocketIdentity(source.Socket)
			if err != nil {
				return err
			}
			saved := journal
			if err := callbacks.Reserve(ctx, path, digest); err != nil {
				return err
			}
			journal, err = load()
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(saved, journal) {
				return errors.New("fork saved recovery journal changed")
			}
			state, err = callbacks.ReadState()
			if err != nil {
				return err
			}
			if err := validateProviderForkReservation(state, source, journal, catalog); err != nil {
				return err
			}
			if err := requireProviderForkEpoch(source.Socket, epoch); err != nil {
				return err
			}
			attemptEpoch = epoch
		}
		if slices.ContainsFunc(journal.Participants, func(p codexproviders.ForkParticipant) bool { return p.State == "not-attempted" }) {
			if err := callbacks.Reserve(ctx, path, digest); err != nil {
				return err
			}
		}
		if err := recheckProviderForkScope(ctx, client, source, stateRoot, authority, callbacks.VerifyAuthority, attemptEpoch); err != nil {
			return err
		}
		for n := range journal.Participants {
			p := &journal.Participants[n]
			savedVerification := p.VerificationDigest
			if p.State == "verified" {
				var proof string
				if _, err := readProviderForkJSON(fmt.Sprintf("%s.proof-%d", path, n), &proof); err != nil {
					return err
				}
				if providerForkDigest([]byte(proof)) != savedVerification {
					return errors.New("fork saved verification changed")
				}
			}
			liveIntent := false
			if p.State == "not-attempted" {
				if n > 0 {
					if err := callbacks.Reserve(ctx, path, digest); err != nil {
						return err
					}
					if err := recheckProviderForkScope(ctx, client, source, stateRoot, authority, callbacks.VerifyAuthority, attemptEpoch); err != nil {
						return err
					}
				}
				if n == 0 {
					if err := callbacks.FirstIntent(ctx, path, digest); err != nil {
						return err
					}
					journal, err = load()
					if err != nil {
						return err
					}
					p = &journal.Participants[n]
					if p.State != "intent" || journal.Stage != "forking" {
						return errors.New("first fork intent was not durably acknowledged")
					}
					state, err = callbacks.ReadState()
					if err != nil {
						return err
					}
					if err := validateProviderForkReservation(state, source, journal, catalog); err != nil {
						return err
					}
					if state.Operation.Stage != "forking" || state.Operation.Phase != "applying" {
						return errors.New("first fork cutoff state was not acknowledged")
					}
				} else {
					p.State = "intent"
					journal.Stage = "forking"
					if err := writeProviderForkJournal(journalPath, journal, source, catalog); err != nil {
						return err
					}
				}
				liveIntent = true
			}
			var effective map[string]any
			if p.State == "intent" {
				if !liveIntent {
					return errors.New("fork intent cannot authorize another native request")
				}
				params, err := providerResumeParams(source.Participants[n].Thread, source.State.Operation.Target)
				if err != nil {
					return err
				}
				if err := requireProviderForkEpoch(source.Socket, attemptEpoch); err != nil {
					return err
				}
				if err := client.Request(ctx, "thread/fork", params, &effective); err != nil {
					return fmt.Errorf("fork outcome unknown: %w", err)
				}
				thread, _ := effective["thread"].(map[string]any)
				child, _ := thread["id"].(string)
				if child == "" || strings.ContainsAny(child, "\x00\t\r\n ") || len(child) > 512 {
					return errors.New("fork returned no usable exact child; outcome unknown")
				}
				p.ChildThreadID, p.State = child, "known-child"
				journal.Stage = "verifying"
				if err := writeProviderForkJournal(journalPath, journal, source, catalog); err != nil {
					return err
				} // Before every dependent read.
			} else {
				if err := requireProviderForkEpoch(source.Socket, attemptEpoch); err != nil {
					return err
				}
				if err := client.Request(ctx, "thread/resume", map[string]any{"threadId": p.ChildThreadID, "excludeTurns": true}, &effective); err != nil {
					return err
				}
			}
			// Wrong/ancestor IDs remain durably known, but cannot receive a title
			// projection or become verified. Retain every original and current ID.
			for _, binding := range source.State.Bindings {
				for original, bound := range binding.Conversations {
					if p.ChildThreadID == original || p.ChildThreadID == bound.ThreadID {
						return errors.New("known fork child aliases retained source identity")
					}
				}
			}
			for other, row := range journal.Participants {
				if other != n && p.ChildThreadID == row.ChildThreadID {
					return errors.New("known fork children alias each other")
				}
			}
			proof, err := verifyProviderForkChild(ctx, client, source.Participants[n], p.ChildThreadID, source.State.Operation.Target, effective, source.Socket, attemptEpoch, savedVerification)
			if err != nil {
				return err
			}
			if savedVerification != "" {
				continue
			} // Preserve verified proof bytes and journal state.
			if err := providerAtomicJSON(fmt.Sprintf("%s.proof-%d", path, n), proof); err != nil {
				return err
			}
			p.VerificationDigest, p.State = providerForkDigest([]byte(proof)), "verified"
			if err := writeProviderForkJournal(journalPath, journal, source, catalog); err != nil {
				return err
			}
		}
		if err := recheckProviderForkScope(ctx, client, source, stateRoot, authority, callbacks.VerifyAuthority, attemptEpoch); err != nil {
			return err
		}
		proofs := []string{}
		for n, p := range journal.Participants {
			var proof string
			_, err := readProviderForkJSON(fmt.Sprintf("%s.proof-%d", path, n), &proof)
			if err != nil {
				return err
			}
			if providerForkDigest([]byte(proof)) != p.VerificationDigest {
				return errors.New("fork verification bytes changed")
			}
			proofs = append(proofs, proof)
		}
		verified := map[string]any{"schema": 1, "strategy": "fork-v1", "operationId": source.OperationID, "workspace": source.Workspace, "root": source.Root, "socket": source.Socket, "catalogDigest": source.CatalogDigest, "sourcePackage": source.SourcePackage, "sourceSnapshotDigest": digest, "proofs": proofs}
		verifiedPath := path + ".verified"
		verifiedBytes, err := json.Marshal(verified)
		if err != nil {
			return err
		}
		if old, err := os.ReadFile(verifiedPath); err == nil {
			if !bytes.Equal(old, verifiedBytes) {
				return errors.New("frozen verified fork proof changed")
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err := providerAtomicJSON(verifiedPath, verified); err != nil {
				return err
			}
		} else {
			return err
		}
		return finishProviderForkProjections(ctx, source, path, stateRoot, catalog, callbacks, load, attemptEpoch)
	})
}

func finishProviderForkProjections(ctx context.Context, source providerForkSnapshot, path, stateRoot string, catalog codexproviders.Catalog, callbacks providerForkCallbacks, load func() (codexproviders.ForkJournal, error), expectedEpoch string) error {
	if err := requireProviderForkEpoch(source.Socket, expectedEpoch); err != nil {
		return err
	}
	verifiedBytes, err := os.ReadFile(path + ".verified")
	if err != nil {
		return err
	}
	digest := providerForkDigest(verifiedBytes)
	// The first acknowledged write is a checkpoint, never final success. A lost
	// acknowledgement stops this attempt before any Go-owned projection effects.
	if err := requireProviderForkEpoch(source.Socket, expectedEpoch); err != nil {
		return err
	}
	if err := callbacks.Commit(ctx, path+".verified", digest); err != nil {
		return err
	}
	read := func() (codexproviders.State, codexproviders.ForkJournal, error) {
		if err := requireProviderForkEpoch(source.Socket, expectedEpoch); err != nil {
			return codexproviders.State{}, codexproviders.ForkJournal{}, err
		}
		state, err := callbacks.ReadState()
		if err != nil {
			return state, codexproviders.ForkJournal{}, err
		}
		journal, err := load()
		if err != nil {
			return state, journal, err
		}
		return state, journal, verifyProviderForkCommit(state, journal, source, path, catalog)
	}
	state, journal, err := read()
	if err != nil {
		return err
	}
	complete := func() bool {
		if state.Operation.Phase != "applied" {
			return false
		}
		for _, p := range journal.Projections {
			if !p.Manifest || !p.Authority || !p.Recovery || !p.Terminal {
				return false
			}
		}
		return true
	}
	if complete() {
		return nil
	}
	for _, slug := range source.Sessions {
		projection := journal.Projections[slug]
		if !projection.Manifest || !projection.Authority {
			return errors.New("child authority projection is incomplete")
		}
		if projection.Recovery {
			continue
		}
		for _, p := range source.Participants {
			if p.Current.Key.Slug != slug {
				continue
			}
			var child string
			for _, row := range journal.Participants {
				if row.Current.Key == p.Current.Key {
					child = row.ChildThreadID
				}
			}
			if child == "" || p.BrowserRecords == nil {
				return errors.New("known child browser proof is missing")
			}
			if err := web.ProjectProviderForkBrowserRecords(stateRoot, source.Root, slug, p.Current.Key.RootThreadID, p.Thread.ID, child, *p.BrowserRecords); err != nil {
				return err
			}
		}
		if err := (session.RecoveryStore{Root: stateRoot, Workspace: source.Root}).ProjectCodexBinding(ctx, slug, source.Rosters[slug].Root, source.Socket); err != nil {
			return err
		}
		// Reconcile exact affected bindings after effects; unrelated global
		// registrations remain valid and are never overwritten by this journal.
		state, journal, err = read()
		if err != nil {
			return err
		}
		projection = journal.Projections[slug]
		projection.Recovery = true
		journal.Projections[slug] = projection
		if err := writeProviderForkJournal(path+".journal", journal, source, catalog); err != nil {
			return err
		}
	}
	// Same action, step and immutable proof. Ruby now owns terminal completion
	// and the sole applied state write; this is a required second phase, no loop.
	if err := requireProviderForkEpoch(source.Socket, expectedEpoch); err != nil {
		return err
	}
	if err := callbacks.Commit(ctx, path+".verified", digest); err != nil {
		return err
	}
	state, journal, err = read()
	if err != nil {
		return err
	}
	if !complete() {
		return errors.New("fork projections remain incomplete")
	}
	return nil
}

func verifyProviderForkCommit(state codexproviders.State, journal codexproviders.ForkJournal, source providerForkSnapshot, path string, catalog codexproviders.Catalog) error {
	if err := codexproviders.ValidateBindings(state, catalog); err != nil {
		return err
	}
	if state.Operation == nil || state.Operation.ID != journal.OperationID || state.Operation.Stage != "restoring-terminals" || journal.Stage != "restoring-terminals" {
		return errors.New("fork collective publication remains uncertain")
	}
	var sealed codexproviders.BindingEvidence
	sealBytes, err := readProviderForkJSON(path+".evidence", &sealed)
	if err != nil {
		return err
	}
	verifiedBytes, err := os.ReadFile(path + ".verified")
	if err != nil {
		return err
	}
	if sealed.OperationID != journal.OperationID || sealed.SnapshotDigest != providerForkDigest(verifiedBytes) || len(sealed.Participants) != len(journal.Participants) {
		return errors.New("fork collective evidence changed")
	}
	selection := journal.Selection
	if selection.Scope == "workspace" && state.Active != selection.Provider || selection.Scope == "session" && selection.Inherit && (state.Active != selection.Provider || state.Sessions[selection.Session].Provider != "") || selection.Scope == "session" && !selection.Inherit && state.Sessions[selection.Session] != (codexproviders.Selection{Provider: selection.Provider, RootThreadID: state.Operation.Roots[selection.Session]}) {
		return errors.New("fork collective selection changed")
	}
	for n, p := range journal.Participants {
		b, ok := state.Bindings[p.Current.Key.Slug]
		c, found := b.Conversations[p.Current.Key.OriginalThreadID]
		row := sealed.Participants[n]
		if !ok || !found || c.ThreadID != p.ChildThreadID || c.Provider != selection.Provider || c.OperationID != journal.OperationID || c.EvidenceDigest != providerForkDigest(sealBytes) || b.Revision != sealed.CommittedBindingRevision || b.RootThreadID != p.Current.Key.RootThreadID || b.Incarnation != p.Current.Key.Incarnation || row.Key != p.Current.Key || row.OldThreadID != p.SourceThreadID || row.NewThreadID != p.ChildThreadID || row.OldProvider != p.Current.Provider || row.NewProvider != selection.Provider {
			return errors.New("fork collective binding differs from sealed targets")
		}
		_, err := state.ReceiptTargets(p.Current.Key, true, func(op string) ([]byte, error) {
			return os.ReadFile(filepath.Join(filepath.Dir(path), op+".snapshot.json.evidence"))
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type scopedThreadIntent struct {
	Active       bool   `json:"active"`
	GoalActive   bool   `json:"goalActive"`
	GoalOwned    bool   `json:"goalOwned"`
	CloseIntent  bool   `json:"closeIntent"`
	Closed       bool   `json:"closed"`
	Verified     bool   `json:"verified"`
	Continued    bool   `json:"continued"`
	Continuation string `json:"continuation"`
	PauseTurnID  string `json:"pauseTurnId,omitempty"`
}

type scopedProviderSnapshot struct {
	providerInventory
	Operation string                         `json:"operation"`
	Target    string                         `json:"target"`
	Sessions  []string                       `json:"sessions"`
	Intents   map[string]*scopedThreadIntent `json:"intents"`
}

type scopedProviderProgress struct {
	Operation   string `json:"operation"`
	Stage       string `json:"stage"`
	Thread      string `json:"thread,omitempty"`
	ThreadLabel string `json:"threadLabel,omitempty"`
	Completed   int    `json:"completed"`
	Total       int    `json:"total"`
	UpdatedAt   string `json:"updatedAt"`
	Reason      string `json:"reason,omitempty"`
}

type scopedProviderFailure struct {
	Reason string
	Err    error
}

func (e *scopedProviderFailure) Error() string { return e.Err.Error() }
func (e *scopedProviderFailure) Unwrap() error { return e.Err }

func providerAtomicJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 16*1024*1024 {
		return errors.New("provider snapshot exceeds bounds")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".provider-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func scopedProviderCommand(ctx context.Context, client *workspacecodex.Client, action, workspace, stateRoot, socket, profile, authority, path, target, operation, scopes, rootsJSON string) (resultErr error) {
	if !canonicalAbsolutePath(authority) || !canonicalAbsolutePath(path) || !queueOperationID(operation) {
		return errors.New("scoped provider helper requires exact private identities")
	}
	if action == "scoped-prepare" {
		var stop context.CancelFunc
		ctx, stop = providerPreparationContext(ctx, path, operation)
		defer stop()
	}
	slugs := []string{}
	if scopes != "" {
		slugs = strings.Split(scopes, ",")
	}
	slices.Sort(slugs)
	scope := map[string]bool{}
	for _, slug := range slugs {
		if !session.ValidSlug(slug) || scope[slug] {
			return errors.New("invalid scoped provider session")
		}
		scope[slug] = true
	}
	var roots map[string]string
	if json.Unmarshal([]byte(rootsJSON), &roots) != nil || len(roots) != len(slugs) {
		return errors.New("scoped provider roots are absent or inconsistent")
	}
	progress := scopedProviderProgress{Operation: operation, Stage: strings.TrimPrefix(action, "scoped-")}
	labels := map[string]string{}
	writeProgress := func(stage, thread string, completed, total int) error {
		progress.Stage, progress.Thread, progress.Completed, progress.Total = stage, thread, completed, total
		progress.ThreadLabel = labels[thread]
		progress.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		return providerAtomicJSON(path+".progress", progress)
	}
	defer func() {
		if resultErr != nil {
			var failure *scopedProviderFailure
			if errors.As(resultErr, &failure) {
				progress.Reason = failure.Reason
			}
			// Stage + exact thread locate a failure without disclosing native
			// messages, endpoint addresses, input text or credentials.
			if progress.Reason == "" {
				progress.Reason = "native_" + progress.Stage + "_failed"
			}
			progress.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			_ = providerAtomicJSON(path+".progress", progress)
		}
	}()
	if err := writeProgress("locking-sessions", "", 0, len(slugs)); err != nil {
		return err
	}
	for _, slug := range slugs {
		var lock *session.RuntimeLock
		for {
			var err error
			lock, err = session.LockRuntimeExclusive(authority, slug)
			if err == nil {
				break
			}
			if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
				return err
			}
			progress.Thread, progress.Reason = slug, "session_runtime_busy"
			progress.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if err := providerAtomicJSON(path+".progress", progress); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		progress.Reason = ""
		defer lock.Close()
		owner, err := session.PendingLifecycle(workspace, slug)
		if err != nil || owner != "" {
			progress.Thread, progress.Reason = slug, "session_lifecycle_unfinished"
			return errors.New("scoped provider session has an unfinished lifecycle operation")
		}
		summary, err := session.Find(workspace, slug)
		if err != nil || summary.Archived || roots[slug] == "" || summary.Codex.ThreadID != roots[slug] || summary.Codex.SocketPath != socket {
			progress.Thread, progress.Reason = slug, "session_identity_changed"
			return errors.New("scoped provider retained session root changed")
		}
	}
	if action == "scoped-prepare" {
		for _, slug := range slugs {
			if err := client.RetryStore().CancelSession(ctx, slug, roots[slug], socket, client.RetryBackend()); err != nil {
				return err
			}
		}
		inventory, err := buildProviderInventoryScope(ctx, client, workspace, stateRoot, socket, profile, scope)
		if err != nil {
			return err
		}
		snapshot := scopedProviderSnapshot{providerInventory: inventory, Operation: operation, Target: target, Sessions: slugs, Intents: map[string]*scopedThreadIntent{}}
		for index := range snapshot.Threads {
			thread := &snapshot.Threads[index]
			labels[thread.ID] = providerThreadLabel(*thread)
			if err := writeProgress("capturing-settings", thread.ID, index, len(snapshot.Threads)); err != nil {
				return err
			}
			turn, err := readScopedProviderTurn(ctx, client, thread.ID)
			if err != nil {
				return err
			}
			goal, err := client.HasActiveGoal(ctx, thread.ID)
			if err != nil {
				return err
			}
			if err := captureScopedProviderThread(ctx, client, thread, target); err != nil {
				return err
			}
			digest := sha256.Sum256([]byte(operation + ":" + thread.ID))
			identity := hex.EncodeToString(digest[:16])
			snapshot.Intents[thread.ID] = &scopedThreadIntent{Active: turn.Status == "inProgress", PauseTurnID: turn.ID, GoalActive: goal, GoalOwned: goal, Continuation: identity[0:8] + "-" + identity[8:12] + "-" + identity[12:16] + "-" + identity[16:20] + "-" + identity[20:32]}
		}
		return json.NewEncoder(os.Stdout).Encode(snapshot)
	}
	var snapshot scopedProviderSnapshot
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16*1024*1024 {
		return errors.New("scoped provider snapshot is not private and bounded")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, &snapshot) != nil || snapshot.Schema != 1 || snapshot.Workspace != workspace || snapshot.Operation != operation || snapshot.Target != target || !slices.Equal(snapshot.Sessions, slugs) || len(snapshot.Threads) > 4096 {
		return errors.New("scoped provider snapshot identity changed")
	}
	for _, thread := range snapshot.Threads {
		labels[thread.ID] = providerThreadLabel(thread)
		if !scope[thread.Slug] || thread.Cwd != filepath.Join(workspace, "work", thread.Slug) || snapshot.Intents[thread.ID] == nil {
			return errors.New("scoped provider thread has a foreign session")
		}
		if err := validateProviderResumePayloads(thread, target); err != nil {
			return err
		}
	}
	if action == "scoped-switch" {
		for index, thread := range snapshot.Threads {
			intent := snapshot.Intents[thread.ID]
			if intent.Verified {
				continue
			}
			if err := writeProgress("pausing", thread.ID, index, len(snapshot.Threads)); err != nil {
				return err
			}
			if !intent.Closed {
				if err := writeProgress("closing", thread.ID, index, len(snapshot.Threads)); err != nil {
					return err
				}
				archived, err := scopedThreadArchived(ctx, client, thread.ID)
				if err != nil {
					return err
				}
				if !archived {
					turn, err := readScopedProviderTurn(ctx, client, thread.ID)
					if err != nil {
						return err
					}
					// A failed archive can stop execution before moving the history.
					// Preserve the journaled active flag for that same stopped turn.
					if !intent.CloseIntent || turn.ID != intent.PauseTurnID || turn.Status == "inProgress" {
						goal, err := client.HasActiveGoal(ctx, thread.ID)
						if err != nil {
							return err
						}
						intent.GoalOwned = goal || (intent.GoalOwned && intent.PauseTurnID == turn.ID)
						intent.Active, intent.PauseTurnID = turn.Status == "inProgress", turn.ID
					}
					intent.CloseIntent = true
					if err := providerAtomicJSON(path, snapshot); err != nil {
						return err
					}
					var response any
					if err := client.Request(ctx, "thread/archive", map[string]any{"threadId": thread.ID}, &response); err != nil {
						return err
					}
				}
				intent.Closed = true
				if err := providerAtomicJSON(path, snapshot); err != nil {
					return err
				}
			}
			turn, err := readScopedProviderTurn(ctx, client, thread.ID)
			if err != nil {
				return err
			}
			if turn.ID != intent.PauseTurnID {
				// Goal completion may release ordinary queued work between capture
				// and shutdown. The old goal cannot own that newer source turn.
				intent.GoalOwned = false
			}
			intent.Active = turn.Status == "interrupted" && (intent.Active || turn.ID != intent.PauseTurnID)
			if err := providerAtomicJSON(path, snapshot); err != nil {
				return err
			}
		}
		// Native teardown stops the selected root's temporary descendants.
		// Only retained leads/members are reopened and checked for target routing.
		for index, thread := range snapshot.Threads {
			intent := snapshot.Intents[thread.ID]
			if err := writeProgress("reopening", thread.ID, index, len(snapshot.Threads)); err != nil {
				return err
			}
			archived, err := scopedThreadArchived(ctx, client, thread.ID)
			if err != nil {
				return err
			}
			if archived {
				var response any
				if err := client.Request(ctx, "thread/unarchive", map[string]any{"threadId": thread.ID}, &response); err != nil {
					return err
				}
			}
			params, err := providerResumeParams(thread, target)
			if err != nil {
				return err
			}
			var effective map[string]any
			if err := client.Request(ctx, "thread/resume", params, &effective); err != nil {
				return err
			}
			if err := validateProviderResponse(thread, effective, target); err != nil {
				return err
			}
			update := map[string]any{"threadId": thread.ID}
			for _, key := range []string{"disabledPluginIds", "collaborationMode", "multiAgentMode"} {
				if value, saved := thread.Original[key]; saved && !reflect.DeepEqual(value, effective[key]) {
					update[key] = value
				}
			}
			if len(update) > 1 {
				var response any
				if err := client.Request(ctx, "thread/settings/update", update, &response); err != nil {
					return err
				}
				if err := client.Request(ctx, "thread/resume", params, &effective); err != nil {
					return err
				}
				for key, value := range update {
					if key != "threadId" && !reflect.DeepEqual(value, effective[key]) {
						return fmt.Errorf("scoped provider changed retained %s", key)
					}
				}
			}
			for _, key := range []string{"runtimeWorkspaceRoots", "approvalPolicy", "approvalsReviewer", "sandbox", "activePermissionProfile", "serviceTier"} {
				if key == "serviceTier" && providerTierMatches(thread.Original[key], effective[key]) {
					continue
				}
				if !reflect.DeepEqual(thread.Original[key], effective[key]) {
					return fmt.Errorf("scoped provider changed retained %s", key)
				}
			}
			// Native goal reads cannot locate an unloaded archived history.
			// Cold resume restores current durable state; inspect it only now.
			intent.GoalActive, err = client.HasActiveGoal(ctx, thread.ID)
			if err != nil {
				return err
			}
			intent.Verified = true
			if err := providerAtomicJSON(path, snapshot); err != nil {
				return err
			}
		}
	} else if action == "scoped-continue" {
		for index, thread := range snapshot.Threads {
			intent := snapshot.Intents[thread.ID]
			if !intent.Verified {
				return errors.New("provider continuation requires effective routing proof")
			}
			if intent.Continued {
				continue
			}
			if err := writeProgress("resuming-work", thread.ID, index, len(snapshot.Threads)); err != nil {
				return err
			}
			// Native cold resume restores active goals from durable current
			// state. Do not replay goal/set: that could reactivate completed
			// work after an uncertain response. Non-goal work uses a receipt.
			if intent.Active && !intent.GoalOwned && !intent.GoalActive {
				if err := client.ConfirmRetryBinding(ctx, thread.ID, thread.Settings.Policy); err != nil {
					return err
				}
				if _, err := client.Send(ctx, thread.ID, "Continue the work interrupted by the provider switch. Check the last tool results before retrying any action. Ask again for any unanswered question or approval.", intent.Continuation, "provider-switch:"+operation); err != nil {
					return err
				}
			}

			intent.Continued = true
			if err := providerAtomicJSON(path, snapshot); err != nil {
				return err
			}
		}
	} else {
		return errors.New("unknown scoped provider operation")
	}
	if err := writeProgress("complete", "", len(snapshot.Threads), len(snapshot.Threads)); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": 1, "verified": true})
}

func providerThreadLabel(thread providerThread) string {
	if thread.MemberAddress != "" {
		return thread.MemberAddress
	}
	return "Lead"
}

func queueOperationID(value string) bool {
	return len(value) == 36 && strings.Count(value, "-") == 4 && strings.Trim(value, "0123456789abcdef-") == ""
}

type scopedProviderTurn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func readScopedProviderTurn(ctx context.Context, client *workspacecodex.Client, thread string) (scopedProviderTurn, error) {
	var page struct {
		Data *[]scopedProviderTurn `json:"data"`
	}
	if err := client.Request(ctx, "thread/turns/list", map[string]any{"threadId": thread, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}, &page); err != nil {
		return scopedProviderTurn{}, err
	}
	if page.Data == nil || len(*page.Data) > 1 {
		return scopedProviderTurn{}, errors.New("native latest turn is absent or ambiguous")
	}
	if len(*page.Data) == 0 {
		return scopedProviderTurn{}, nil
	}
	return (*page.Data)[0], nil
}

func scopedThreadArchived(ctx context.Context, client *workspacecodex.Client, thread string) (bool, error) {
	var response struct {
		Thread struct {
			ID   string  `json:"id"`
			Path *string `json:"path"`
		} `json:"thread"`
	}
	if err := client.Request(ctx, "thread/read", map[string]any{"threadId": thread, "excludeTurns": true}, &response); err != nil {
		return false, err
	}
	if response.Thread.ID != thread || response.Thread.Path == nil {
		return false, errors.New("native close state has no materialized identity")
	}
	return filepath.Base(filepath.Dir(*response.Thread.Path)) == "archived_sessions", nil
}

func captureScopedProviderThread(ctx context.Context, client *workspacecodex.Client, thread *providerThread, target string) error {
	fail := func(reason string, err error) error { return &scopedProviderFailure{Reason: reason, Err: err} }
	var metadata struct {
		Thread struct {
			ID       string         `json:"id"`
			Cwd      string         `json:"cwd"`
			Path     *string        `json:"path"`
			Status   map[string]any `json:"status"`
			Provider string         `json:"modelProvider"`
		} `json:"thread"`
	}
	if err := client.Request(ctx, "thread/read", map[string]any{"threadId": thread.ID, "excludeTurns": true}, &metadata); err != nil {
		return fail("native_metadata_unavailable", err)
	}
	if metadata.Thread.ID != thread.ID || metadata.Thread.Cwd != thread.Cwd || metadata.Thread.Status == nil || metadata.Thread.Provider == "" {
		return fail("thread_identity_unproved", errors.New("native thread metadata has an unexpected identity"))
	}
	params := map[string]any{"threadId": thread.ID, "cwd": thread.Cwd, "excludeTurns": true, "modelProvider": metadata.Thread.Provider, "model": thread.Settings.Model, "config": map[string]any{"model_reasoning_effort": thread.Settings.ReasoningEffort}}
	if metadata.Thread.Status["type"] == "notLoaded" {
		if metadata.Thread.Path == nil {
			return errors.New("cold provider thread has no settings history")
		}
		if err := preserveColdNativePolicy(*metadata.Thread.Path, params); err != nil {
			return fail("cold_permissions_unsupported", err)
		}
	}
	if err := client.Request(ctx, "thread/resume", params, &thread.Original); err != nil {
		return fail("native_settings_resume_failed", err)
	}
	if err := validateProviderResponse(*thread, thread.Original, ""); err != nil {
		return err
	}
	if !supportedProviderPermissionProfile(thread.Original["activePermissionProfile"]) {
		return fail("named_permission_profile_unsupported", errors.New("cold provider transition cannot preserve this named permissions profile"))
	}
	if err := validateProviderResumePayloads(*thread, target); err != nil {
		return fail("retained_policy_mismatch", err)
	}
	return nil
}
