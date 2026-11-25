package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/cluster"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/creationprogress"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"golang.org/x/sys/unix"
)

const sessionLifecycleDeveloperInstructions = "Completing work, preparing a handoff, or setting " +
	"lifecycle state does not authorize archiving, deleting, stopping, finalizing, removing, " +
	"invoking private lifecycle helpers, or scheduling delayed or background cleanup for this " +
	"session. Perform a session lifecycle action only when the user explicitly requests that " +
	"exact action for this exact session in the current conversation; otherwise leave the " +
	"session open."

func newCodexClient(socket, workspace string, stateRoots ...string) *workspacecodex.Client {
	return newCodexClientWithHistory(socket, workspace, nil, stateRoots...)
}

func newCodexClientWithHistory(socket, workspace string, history codex.HistoryReader, stateRoots ...string) *workspacecodex.Client {
	return configuredCodexClient(socket, workspace, history, false, stateRoots...)
}

// Transition proofs never watch or load a thread and cannot answer native requests.
func newTransitionClient(socket, workspace string, stateRoots ...string) *workspacecodex.Client {
	return configuredCodexClient(socket, workspace, nil, true, stateRoots...)
}

func configuredCodexClient(socket, workspace string, history codex.HistoryReader, cold bool, stateRoots ...string) *workspacecodex.Client {
	stateRoot := os.Getenv("DEV_WORKSPACES_STATE")
	if len(stateRoots) > 0 {
		stateRoot = stateRoots[0]
	}
	ledgerPath := socket + ".submission-attempts-v3.json"
	legacyPath := ""
	databasePath := ""
	if stateRoot != "" {
		legacyPath = ledgerPath
		identity := sha256.Sum256([]byte(socket))
		ledgerPath = filepath.Join(stateRoot, "submissions", hex.EncodeToString(identity[:])+".json")
		databasePath = filepath.Join(stateRoot, "submissions", hex.EncodeToString(identity[:])+".sqlite")
	}
	construct := workspacecodex.NewWithOptions
	if cold {
		construct = workspacecodex.NewTransitionClient
	}
	client := construct(socket, workspace, codex.ClientOptions{
		HistoryReader: history,
		ObserverOnly:  cold,
		ClientInfo: codex.ClientInfo{
			Name: "dev-workspace", Title: "Development Workspace", Version: "0.1.0",
		},
		DeveloperInstructions:              sessionLifecycleDeveloperInstructions,
		PreserveThreadInstructionsOnResume: true,
		// Keep the deployed retry identity while making storage ownership explicit.
		SubmissionLedgerPath: ledgerPath, LegacySubmissionLedgerPath: legacyPath,
		SubmissionDatabasePath: databasePath,
		DeliveryObserver:       deliveryDiagnostics,
	})
	client.RecoveryRoot = stateRoot
	if executable, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(executable); err == nil {
			if installed, err := agentteams.LoadInstalled(filepath.Dir(filepath.Dir(real))); err == nil {
				client.RetryCatalog = installed.Catalog
			}
		}
	}
	client.RetryPolicyResolver = func(ctx context.Context, thread string) (codex.ThreadPolicy, error) {
		metadata, err := client.ReadThreadMetadata(ctx, thread, false)
		if err != nil {
			return codex.ThreadPolicy{}, err
		}
		slug := filepath.Base(metadata.Cwd)
		if metadata.ID != thread || !session.ValidSlug(slug) || metadata.Cwd != filepath.Join(workspace, "work", slug) {
			return codex.ThreadPolicy{}, errors.New("managed admission has an unexpected session directory")
		}
		summary, err := session.Find(workspace, slug)
		if err != nil || summary.Archived {
			return codex.ThreadPolicy{}, errors.New("managed admission session is unavailable")
		}
		store, err := teamruntime.NewStore(stateRoot, workspace)
		if err != nil {
			return codex.ThreadPolicy{}, err
		}
		var roster *teamruntime.Roster
		if summary.Codex.ThreadID != "" {
			roster, err = store.Load(slug, summary.Codex.ThreadID)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return codex.ThreadPolicy{}, err
		}
		lead := summary.Codex.ThreadID == thread || summary.Codex.ThreadID == ""
		if summary.Codex.BindingSchema == 3 {
			current, err := store.ResolveConversation(slug, summary.Codex.ThreadID, "lead", summary.Codex.ThreadID)
			if err != nil {
				return codex.ThreadPolicy{}, err
			}
			lead = current.ThreadID == thread
		}
		if lead {
			instructions := ""
			if roster != nil {
				instructions = roster.LeadInstructions
			}
			return workspacecodex.LeadThreadPolicy(slug, workspace, instructions), nil
		}
		if roster != nil {
			for _, member := range roster.Members {
				if member.State != "ready" || member.RemovedAt != nil || member.RetireIntent != "" {
					continue
				}
				currentThread := member.Thread
				if summary.Codex.BindingSchema == 3 {
					current, err := store.ResolveConversation(slug, summary.Codex.ThreadID, member.Address, member.Thread)
					if err != nil {
						return codex.ThreadPolicy{}, err
					}
					currentThread = current.ThreadID
				}
				if currentThread == thread {
					service := teamruntime.Service{Store: store, Client: client, Workspace: workspace, Catalog: client.RetryCatalog}
					return service.MemberTurnPolicy(slug, summary.Codex.ThreadID, member)
				}
			}
		}
		return codex.ThreadPolicy{}, errors.New("managed admission is outside the retained team")
	}
	return client
}

func deliveryDiagnostics(event codex.DeliveryEvent) {
	if event.Failed || event.Duration >= 250*time.Millisecond || os.Getenv("DEV_WORKSPACE_DELIVERY_TRACE") == "1" {
		log.Printf("delivery stage=%s thread=%s attempt=%s duration=%s failed=%t", event.Stage, event.ThreadID, event.ClientUserMessageID, event.Duration, event.Failed)
	}
}

func rootThreadSettings(model, effort, slug, workspace, leadInstructions string) codex.ThreadSettings {
	return codex.ThreadSettings{
		Model: model, ReasoningEffort: effort, Policy: workspacecodex.LeadThreadPolicy(slug, workspace, leadInstructions),
	}
}

const version = "0.1.0"

type threadRuntime struct {
	Slug, Workspace, WorkDir, WorktreesDir, PortalBaseURL, PortalURL  string
	AuthorityDir, TmuxSocket, CodexCommand, CodexSocket, CodexVersion string
	PortalCommand                                                     string
}

func (runtime threadRuntime) complete() bool {
	return runtime.Slug != "" && runtime.Workspace != "" && runtime.WorkDir != "" &&
		runtime.WorktreesDir != "" && runtime.PortalBaseURL != "" && runtime.PortalURL != "" &&
		runtime.AuthorityDir != "" && runtime.TmuxSocket != "" && runtime.CodexCommand != "" &&
		runtime.CodexSocket != "" && runtime.CodexVersion != "" && runtime.PortalCommand != ""
}

func (runtime threadRuntime) environment() map[string]string {
	return map[string]string{
		"DEV_SESSION_SLUG":            runtime.Slug,
		"DEV_SESSION_WORKSPACE":       runtime.Workspace,
		"DEV_SESSION_WORK_DIR":        runtime.WorkDir,
		"DEV_SESSION_WORKTREES_DIR":   runtime.WorktreesDir,
		"DEV_SESSION_PORTAL_BASE_URL": runtime.PortalBaseURL,
		"DEV_SESSION_URL":             runtime.PortalURL,
		"DEV_SESSION_AUTHORITY_DIR":   runtime.AuthorityDir,
		"DEV_SESSION_TMUX_SOCKET":     runtime.TmuxSocket,
		"DEV_SESSION_CODEX":           runtime.CodexCommand,
		"DEV_SESSION_CODEX_SOCKET":    runtime.CodexSocket,
		"DEV_SESSION_CODEX_VERSION":   runtime.CodexVersion,
		"DEV_SESSION_PORTAL_COMMAND":  runtime.PortalCommand,
		"DEV_SESSION_REQUIRE_RUNTIME": "1",
	}
}

// Fork uses the existing inherited lock descriptor mechanism with its own
// operation identity. Closing this duplicate never unlocks the parent's OFD.
func inheritedForkParentLock(workspace, authorityDir, slug string) (*os.File, error) {
	if os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") != "fork" || !session.ValidSlug(slug) || !canonicalAbsolutePath(workspace) {
		return nil, errors.New("bound root fork requires its admitted parent")
	}
	fd, err := strconv.Atoi(os.Getenv("DEV_SESSION_LIFECYCLE_LOCK_FD"))
	if err != nil || fd < 3 {
		return nil, errors.New("fork parent descriptor is missing")
	}
	expected := filepath.Join(workspace, "worktrees", ".locks", slug+".lock")
	if authorityDir != "" {
		if !canonicalAbsolutePath(authorityDir) {
			return nil, errors.New("fork parent authority path changed")
		}
		expected = filepath.Join(authorityDir, slug+".lock")
	}
	path := os.Getenv("DEV_SESSION_LIFECYCLE_LOCK_PATH")
	if path != expected || !canonicalAbsolutePath(path) {
		return nil, errors.New("fork parent lock path changed")
	}
	duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, err
	}
	inherited := os.NewFile(uintptr(duplicate), "fork-parent-lock")
	fail := func(err error) (*os.File, error) { inherited.Close(); return nil, err }
	var held, current unix.Stat_t
	if err := unix.Fstat(duplicate, &held); err != nil {
		return fail(err)
	}
	probe, err := unix.Open(path, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	defer unix.Close(probe)
	if err := unix.Fstat(probe, &current); err != nil {
		return fail(err)
	}
	if held.Mode&unix.S_IFMT != unix.S_IFREG || current.Mode&unix.S_IFMT != unix.S_IFREG || held.Mode&0777 != 0600 || current.Mode&0777 != 0600 || held.Uid != uint32(os.Geteuid()) || current.Uid != uint32(os.Geteuid()) || held.Dev != current.Dev || held.Ino != current.Ino {
		return fail(errors.New("fork parent lock identity changed"))
	}
	err = unix.Flock(probe, unix.LOCK_SH|unix.LOCK_NB)
	if err == nil {
		return fail(errors.New("fork parent exclusion is missing"))
	}
	if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
		return fail(err)
	}
	if err := unix.Flock(duplicate, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(errors.New("inherited fork descriptor does not own exclusion"))
	}
	return inherited, nil
}

// Stdin is expected data from the owning Ruby executor, never authority.
type lifecycleCommandInput = session.LifecycleCommandInput

func readLifecycleCommandInput(input io.Reader) (lifecycleCommandInput, error) {
	data, err := io.ReadAll(io.LimitReader(input, (64<<10)+1))
	if err != nil {
		return lifecycleCommandInput{}, err
	}
	return session.DecodeLifecycleCommandInput(data)
}

// Delete/revive prove their own admitted operation. Archive deliberately keeps
// its existing monitor helper unchanged. Closing never unlocks the parent OFD.
func inheritedLifecycleParentLock(workspace, authorityDir, slug, operation string) (*os.File, error) {
	if operation == "archive" {
		return inheritedMonitorArchiveLock(workspace, authorityDir, slug)
	}
	if (operation != "delete" && operation != "revive") || os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") != operation || !session.ValidSlug(slug) || !canonicalAbsolutePath(workspace) {
		return nil, errors.New("lifecycle command has no matching admitted parent")
	}
	fd, err := strconv.Atoi(os.Getenv("DEV_SESSION_LIFECYCLE_LOCK_FD"))
	if err != nil || fd < 3 {
		return nil, errors.New("lifecycle parent descriptor is missing")
	}
	expected := filepath.Join(workspace, "worktrees", ".locks", slug+".lock")
	if authorityDir != "" {
		if !canonicalAbsolutePath(authorityDir) {
			return nil, errors.New("lifecycle authority path changed")
		}
		expected = filepath.Join(authorityDir, slug+".lock")
	}
	if os.Getenv("DEV_SESSION_LIFECYCLE_LOCK_PATH") != expected || !canonicalAbsolutePath(expected) {
		return nil, errors.New("lifecycle parent lock path changed")
	}
	duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, err
	}
	inherited := os.NewFile(uintptr(duplicate), operation+"-parent-lock")
	fail := func(err error) (*os.File, error) { inherited.Close(); return nil, err }
	var held, current unix.Stat_t
	if err := unix.Fstat(duplicate, &held); err != nil {
		return fail(err)
	}
	probe, err := unix.Open(expected, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	defer unix.Close(probe)
	if err := unix.Fstat(probe, &current); err != nil {
		return fail(err)
	}
	if held.Mode&unix.S_IFMT != unix.S_IFREG || current.Mode&unix.S_IFMT != unix.S_IFREG || held.Mode&0777 != 0600 || current.Mode&0777 != 0600 || held.Uid != uint32(os.Geteuid()) || current.Uid != uint32(os.Geteuid()) || held.Dev != current.Dev || held.Ino != current.Ino {
		return fail(errors.New("lifecycle parent lock ownership/inode changed"))
	}
	err = unix.Flock(probe, unix.LOCK_SH|unix.LOCK_NB)
	if err == nil {
		return fail(errors.New("lifecycle parent exclusion is missing"))
	}
	if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
		return fail(err)
	}
	if err := unix.Flock(duplicate, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(errors.New("inherited lifecycle descriptor does not own exclusion"))
	}
	return inherited, nil
}

// The returned duplicate must live through the synchronous command. Native
// selection is checked again while the caller owns the actual team lock.
func admitLifecycleCommand(workspace, stateRoot, authorityDir, slug, socket, nativeCommand string, force *bool) (*teamruntime.LifecycleSelection, *os.File, error) {
	summary, err := session.Find(workspace, slug)
	if errors.Is(err, os.ErrNotExist) && os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") == "" {
		_, ownerErr := portalweb.ValidateLifecycleCommandReceipt(workspace, stateRoot, slug, portalweb.LifecycleCommandExpectation{
			Kind: "archive", OperationID: strings.Repeat("0", 64), NativeCommand: "thread/retire",
		})
		return nil, nil, ownerErr
	}
	if err != nil {
		return nil, nil, err
	}
	progress, err := session.PendingLifecycleProgress(workspace, slug)
	if err != nil {
		return nil, nil, err
	}
	operation := os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION")
	if operation != "archive" && operation != "delete" && operation != "revive" {
		if summary.Codex.BindingSchema == 3 || progress != nil && progress.CodexTargets != nil {
			return nil, nil, errors.New("bound lifecycle requires its saved input and admitted parent")
		}
		// A legacy invocation cannot hide an active browser receipt by omitting ID.
		_, err := portalweb.ValidateLifecycleCommandReceipt(workspace, stateRoot, slug, portalweb.LifecycleCommandExpectation{
			Kind: "archive", OperationID: strings.Repeat("0", 64), OriginalRoot: summary.Codex.ThreadID, NativeCommand: "thread/retire",
		})
		return nil, nil, err
	}
	input, err := readLifecycleCommandInput(os.Stdin)
	if err != nil {
		return nil, nil, err
	}
	parent, err := inheritedLifecycleParentLock(workspace, authorityDir, slug, operation)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*teamruntime.LifecycleSelection, *os.File, error) {
		parent.Close()
		return nil, nil, err
	}
	if summary.Codex.SocketPath != "" && summary.Codex.SocketPath != socket || input.Targets != nil && (summary.Codex.BindingSchema != 3 || summary.Codex.SocketPath != socket) {
		return fail(errors.New("lifecycle original socket/manifest changed"))
	}
	expected := portalweb.LifecycleCommandExpectation{Kind: operation, OperationID: input.OperationID, OriginalRoot: summary.Codex.ThreadID, NativeCommand: nativeCommand, Force: force, Targets: input.Targets}
	receipt, err := portalweb.ValidateLifecycleCommandReceipt(workspace, stateRoot, slug, expected)
	if err != nil {
		return fail(err)
	}
	if progress != nil {
		if progress.Operation != operation || progress.JournalID != input.OperationID || !session.SameCodexTargets(progress.CodexTargets, input.Targets) {
			return fail(errors.New("lifecycle command differs from accepted journal/prelude"))
		}
		if progress.RootRecorded && (progress.RetainedThreadID == nil || *progress.RetainedThreadID != summary.Codex.ThreadID) {
			return fail(errors.New("lifecycle journal original root changed"))
		}
		if operation == "delete" && !receipt {
			// RootRecorded=false supplies no logical root. The deletion owner proves
			// its own retained manifest in PrepareDelete before retirement can proceed.
			if nativeCommand != "team/prepare-delete" && progress.Phase != "thread_retiring" {
				return fail(errors.New("deletion command has no original-root phase proof"))
			}
		}
		if force != nil && operation == "delete" && progress.Force != *force {
			return fail(errors.New("lifecycle force differs from accepted deletion"))
		}
		if operation == "archive" {
			explicit := progress.Mode == "archived"
			if force != nil && *force != explicit {
				return fail(errors.New("lifecycle force differs from accepted archive mode"))
			}
			if (nativeCommand == "thread/quiesce-explicit" || nativeCommand == "team/archive-explicit") && !explicit || nativeCommand == "team/archive-retained" && explicit {
				return fail(errors.New("lifecycle archive command differs from accepted mode"))
			}
		}
		allowed := false
		switch operation {
		case "archive":
			switch nativeCommand {
			case "thread/quiesce-explicit":
				allowed = progress.ServicePrelude
			case "team/archive-explicit":
				allowed = progress.ServicePrelude || progress.Phase == "tracking_committed" || progress.Phase == "tracking_saved"
			case "team/archive-retained", "thread/retire", "team/require-archived":
				allowed = progress.Phase == "tracking_committed" || progress.Phase == "tracking_saved"
			case "team/require-archive-ready", "team/require-archive-identities":
				allowed = progress.Phase != "archived"
			}
		case "delete":
			allowed = nativeCommand == "team/prepare-delete" && (progress.Phase == "validated" || progress.Phase == "thread_retiring") ||
				(nativeCommand == "thread/retire" || nativeCommand == "team/archive" || nativeCommand == "team/retire") && progress.Phase == "thread_retiring"
		case "revive":
			allowed = nativeCommand == "thread/create" && progress.Phase == "runtime_starting" || nativeCommand == "team/revive" && progress.Phase == "runtime_started"
		}
		if !allowed {
			return fail(errors.New("lifecycle native command is outside its accepted phase"))
		}
	} else {
		explicitQuiesce := operation == "archive" && (nativeCommand == "thread/quiesce-explicit" || nativeCommand == "team/archive-explicit")
		readOnlyArchive := operation == "archive" && receipt && (nativeCommand == "team/require-archive-ready" || nativeCommand == "team/require-archive-identities")
		if !explicitQuiesce && !readOnlyArchive {
			return fail(errors.New("lifecycle command has no owning journal phase"))
		}
	}
	// An auxiliary-only crash belongs to Ruby's exact reconstruction path.
	sidecar := filepath.Join(workspace, "worktrees", ".locks", slug+".archive-cleanup.json")
	if info, err := os.Lstat(sidecar); err == nil {
		if info == nil || operation != "archive" || progress == nil || progress.ServicePrelude {
			return fail(errors.New("archive sidecar requires its restored journal"))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if summary.Codex.BindingSchema == 3 {
		if err := monitorArchiveGeneration(stateRoot); err != nil {
			return fail(err)
		}
		profile := os.Getenv("DEV_WORKSPACES_PROFILE")
		if profile == "" {
			profile = filepath.Join(stateRoot, "profile")
		}
		selected, err := filepath.EvalSymlinks(profile)
		if err != nil {
			return fail(err)
		}
		data, err := os.ReadFile(filepath.Join(selected, "share", "workspace-portal", "runtime-contract.json"))
		var contract map[string]json.RawMessage
		if err != nil || session.DecodePiCreationJSON(data, &contract, 64<<10) != nil || string(contract["codexProviderBindingStateSchema"]) != "3" || string(contract["codexProviderForkTransitionPolicy"]) != "1" || string(contract["codexRuntimeAuthoritySchema"]) != "3" {
			return fail(errors.New("lifecycle binding consumers are unavailable"))
		}
		catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
		if err != nil {
			return fail(err)
		}
		state, err := codexproviders.ReadBindingState(stateRoot, filepath.Base(filepath.Dir(socket)), workspace, socket, catalog)
		if err != nil {
			return fail(err)
		}
		binding, ok := state.Bindings[slug]
		if !ok || binding.RootThreadID != summary.Codex.ThreadID || state.BlocksSession(slug, summary.Codex.ThreadID) {
			return fail(errors.New("lifecycle original binding is changed or fenced"))
		}
	}
	return &teamruntime.LifecycleSelection{Operation: operation, Targets: input.Targets.Clone()}, parent, nil
}

type boundRootForkJournal struct {
	Schema          int                     `json:"schema"`
	Slug            string                  `json:"slug"`
	SourceSlug      string                  `json:"source_slug"`
	SourceThreadID  string                  `json:"source_thread_id"`
	State           string                  `json:"state"`
	TmuxIdentity    string                  `json:"tmux_identity"`
	Model           *string                 `json:"model"`
	Effort          *string                 `json:"effort"`
	RequestedModel  *string                 `json:"requested_model"`
	RequestedEffort *string                 `json:"requested_effort"`
	Provider        string                  `json:"provider,omitempty"`
	Sensitive       bool                    `json:"sensitive,omitempty"`
	SourceFork      *teamruntime.SourceFork `json:"source_fork"`
	RootFork        struct {
		State    string `json:"state"`
		ThreadID string `json:"threadId,omitempty"`
	} `json:"root_fork"`
}

func readBoundRootForkJournal(path string) (*boundRootForkJournal, map[string]json.RawMessage, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uint32(os.Geteuid()) || info.Size() > 64<<10 {
		return nil, nil, errors.New("bound root journal is not private and bounded")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var fields map[string]json.RawMessage
	if err := monitoroperation.DecodeMixedPiRecord(data, &fields, 64<<10); err != nil {
		return nil, nil, err
	}
	_, sourcePresent := fields["source_fork"]
	_, rootPresent := fields["root_fork"]
	if !sourcePresent && !rootPresent {
		return nil, fields, nil
	}
	if sourcePresent != rootPresent {
		return nil, nil, errors.New("bound root journal has an incomplete capture/intent pair")
	}
	var journal boundRootForkJournal
	if err := monitoroperation.DecodeMixedPiRecord(data, &journal, 64<<10); err != nil {
		return nil, nil, err
	}
	for _, key := range []string{"schema", "slug", "source_slug", "source_thread_id", "state", "tmux_identity", "model", "effort", "requested_model", "requested_effort", "source_fork", "root_fork"} {
		if _, present := fields[key]; !present {
			return nil, nil, errors.New("bound root journal is incomplete")
		}
	}
	if journal.SourceFork == nil || journal.State != "creating" || !session.ValidSlug(journal.Slug) || !session.ValidSlug(journal.SourceSlug) || journal.Slug == journal.SourceSlug || journal.SourceThreadID == "" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(journal.TmuxIdentity) || (journal.Schema != 1 && journal.Schema != 2) || journal.Schema == 1 && journal.Provider != "" || journal.Schema == 2 && !codexproviders.ValidID(journal.Provider) {
		return nil, nil, errors.New("bound root journal identity changed")
	}
	var progress map[string]json.RawMessage
	if err := json.Unmarshal(fields["root_fork"], &progress); err != nil {
		return nil, nil, err
	}
	switch journal.RootFork.State {
	case "not-attempted", "intent":
		if len(progress) != 1 || journal.RootFork.ThreadID != "" {
			return nil, nil, errors.New("invalid unconfirmed root fork")
		}
	case "known":
		if len(progress) != 2 || journal.RootFork.ThreadID == "" || journal.RootFork.ThreadID == journal.SourceFork.ThreadID || len(journal.RootFork.ThreadID) > 512 || strings.ContainsAny(journal.RootFork.ThreadID, "\x00\t\r\n ") {
			return nil, nil, errors.New("invalid known root fork")
		}
	default:
		return nil, nil, errors.New("unknown root fork state")
	}
	return &journal, fields, nil
}
func rootForkImmutable(fields map[string]json.RawMessage) ([]byte, error) {
	copy := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		if key != "root_fork" {
			copy[key] = value
		}
	}
	return json.Marshal(copy)
}
func checkpointRootFork(path string, before map[string]json.RawMessage, expected, state, thread string) (map[string]json.RawMessage, error) {
	if state == "known" && (thread == "" || len(thread) > 512 || strings.ContainsAny(thread, "\x00\t\r\n ")) {
		return nil, errors.New("invalid SDK root result remains unknown")
	}

	journal, current, err := readBoundRootForkJournal(path)
	if err != nil {
		return nil, err
	}
	if journal == nil || journal.RootFork.State != expected {
		return nil, errors.New("root fork progress changed")
	}
	old, err := rootForkImmutable(before)
	if err != nil {
		return nil, err
	}
	fresh, err := rootForkImmutable(current)
	if err != nil || !bytes.Equal(old, fresh) {
		return nil, errors.New("root fork immutable request changed")
	}
	progress := map[string]string{"state": state}
	if state == "known" {
		progress["threadId"] = thread
	}
	current["root_fork"], err = json.Marshal(progress)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(current)
	if err != nil || len(encoded) > 64<<10 {
		return nil, errors.New("root fork journal exceeds 64 KiB")
	}
	if err := providerAtomicJSON(path, current); err != nil {
		return nil, err
	}
	after, fields, err := readBoundRootForkJournal(path)
	if err != nil || after == nil || after.RootFork.State != state || after.RootFork.ThreadID != thread {
		return nil, errors.New("root fork checkpoint acknowledgement is unverified")
	}
	return fields, nil
}

// The Ruby parent owns destination runtime and source runtime/team exclusion;
// this synchronous command owns only its destination team lock and subrecord.
func acceptedRootFork(ctx context.Context, client *workspacecodex.Client, runtime threadRuntime, stateRoot, sourceID string, settings codex.ThreadSettings) (string, bool, func(), error) {
	path := filepath.Join(runtime.Workspace, "worktrees", ".locks", runtime.Slug+".fork.json")
	journal, fields, err := readBoundRootForkJournal(path)
	if err != nil {
		return "", true, nil, err
	}
	if journal == nil {
		return "", false, nil, nil
	}
	parent, err := inheritedForkParentLock(runtime.Workspace, runtime.AuthorityDir, runtime.Slug)
	if err != nil {
		return "", true, nil, err
	}
	store, err := teamruntime.NewStore(stateRoot, runtime.Workspace)
	if err != nil {
		parent.Close()
		return "", true, nil, err
	}
	unlock, err := store.TryLockOperation(runtime.Slug)
	if err != nil {
		parent.Close()
		return "", true, nil, err
	}
	release := func() { unlock(); parent.Close() }
	journal, fields, err = readBoundRootForkJournal(path)
	if err != nil || journal == nil {
		return "", true, release, errors.New("bound root journal changed after lock")
	}
	selected, err := filepath.EvalSymlinks(filepath.Join(stateRoot, "profile"))
	if err != nil {
		return "", true, release, err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", true, release, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil || filepath.Dir(filepath.Dir(executable)) != selected {
		return "", true, release, errors.New("bound root executor is not selected package")
	}
	portal, err := filepath.EvalSymlinks(runtime.PortalCommand)
	if err != nil || portal != filepath.Join(selected, "bin", "workspace-portal") {
		return "", true, release, errors.New("bound root command package changed")
	}
	data, err := os.ReadFile(filepath.Join(selected, "share", "workspace-portal", "runtime-contract.json"))
	var contract map[string]json.RawMessage
	if err != nil || session.DecodePiCreationJSON(data, &contract, 64<<10) != nil || string(contract["codexProviderBindingStateSchema"]) != "3" || string(contract["codexProviderForkTransitionPolicy"]) != "1" || string(contract["codexRuntimeAuthoritySchema"]) != "3" {
		return "", true, release, errors.New("bound root consumers are unavailable")
	}
	model, effort := "", ""
	if journal.Model != nil {
		model = *journal.Model
	}
	if journal.Effort != nil {
		effort = *journal.Effort
	}
	if journal.Slug != runtime.Slug || journal.SourceFork.ThreadID != sourceID || runtime.WorkDir != filepath.Join(runtime.Workspace, "work", runtime.Slug) || model != settings.Model || effort != settings.ReasoningEffort {
		return "", true, release, errors.New("bound root invocation differs from saved request")
	}
	if journal.RootFork.State == "intent" {
		return "", true, release, errors.New("root fork outcome is unknown; no discovery or replay is permitted")
	}
	if journal.RootFork.State == "known" {
		child, err := client.ReadThreadMetadata(ctx, journal.RootFork.ThreadID, false)
		if err != nil {
			return "", true, release, err
		}
		if child.ID != journal.RootFork.ThreadID || child.Cwd != runtime.WorkDir || child.ForkedFromID != sourceID || child.Source != "vscode" || child.Ephemeral == nil || *child.Ephemeral {
			return "", true, release, errors.New("known root child identity changed")
		}
		return child.ID, true, release, nil
	}
	capture, err := store.CaptureSourceFork(journal.SourceSlug, journal.SourceThreadID)
	if err != nil {
		return "", true, release, err
	}
	if !reflect.DeepEqual(capture, journal.SourceFork) {
		return "", true, release, errors.New("bound root source capture drifted")
	}
	current, err := store.ResolveHistoryConversation(journal.SourceSlug, journal.SourceThreadID, "lead", journal.SourceThreadID)
	if err != nil {
		return "", true, release, err
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		return "", true, release, err
	}
	state, err := codexproviders.ReadBindingState(stateRoot, filepath.Base(filepath.Dir(runtime.CodexSocket)), runtime.Workspace, runtime.CodexSocket, catalog)
	if err != nil {
		return "", true, release, err
	}
	if state.BlocksSession(runtime.Slug, "") {
		return "", true, release, errors.New("bound root destination is fenced")
	}
	provider := state.Active
	if journal.Provider != "" {
		selection, ok := state.Sessions[runtime.Slug]
		if !ok || selection.Provider != journal.Provider || selection.CreationID != journal.TmuxIdentity {
			return "", true, release, errors.New("bound root destination selection changed")
		}
		provider = selection.Provider
	} else if _, ok := state.Sessions[runtime.Slug]; ok {
		return "", true, release, errors.New("bound root inherited selection changed")
	}
	fields, err = checkpointRootFork(path, fields, "not-attempted", "intent", "")
	if err != nil {
		return "", true, release, err
	}
	digest := sha256.Sum256(fields["source_fork"])
	proof := workspacecodex.AcceptedBoundFork{SourceKey: current.Key, SourceThreadID: sourceID, SourceRevision: current.Revision, SourceProvider: current.Provider, DestinationSlug: runtime.Slug, DestinationIncarnation: journal.TmuxIdentity, DestinationProvider: provider, SourceForkDigest: hex.EncodeToString(digest[:]), SourcePackage: selected}
	id, err := client.ForkAcceptedBoundSource(ctx, proof, runtime.environment(), settings)
	if err != nil {
		return "", true, release, err
	}
	if _, err := checkpointRootFork(path, fields, "intent", "known", id); err != nil {
		return "", true, release, err
	}
	return id, true, release, nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "workspace-portal:", err)
		var transport *codex.TransportError
		if errors.As(err, &transport) {
			os.Exit(75)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: workspace-portal serve|router|thread|team|team-mcp|team-preset|agent-teams|pi-providers|validate|version")
	}
	switch args[0] {
	case "pi-runtime":
		return handlePiRuntime(args[1:])
	case "serve":
		return serve(args[1:])
	case "coordination-notify":
		return coordinationNotify(args[1:])
	case "router":
		return routeWorkspaces(args[1:])
	case "session":
		return sessionCommand(args[1:])
	case "recovery":
		return recoveryCommand(args[1:])
	case "thread":
		return threadCommand(args[1:])
	case "communication":
		return communicationCommand(args[1:])
	case "monitor":
		return monitorCommand(args[1:])
	case "monitor-lifecycle":
		return monitorLifecycleCommand(args[1:])
	case "fork":
		return piForkCommand(args[1:])
	case "team":
		return teamCommand(args[1:])
	case "team-mcp":
		return teamMCPCommand(args[1:], os.Stdin, os.Stdout, os.Stderr, teamMCPDependencies{})
	case "team-preset":
		return teamPresetCommand(args[1:])
	case "agent-teams":
		return agentTeamsCommand(args[1:])
	case "codex-provider":
		return codexProviderCommand(args[1:])
	case "maintenance-control":
		return maintenanceControlCommand(args[1:], os.Stdin, os.Stdout)
	case "capture-comparison":
		return captureComparisonCommand(args[1:])
	case "uploads":
		return uploadCommand(args[1:])
	case "pi-providers":
		return piProvidersCommand(args[1:])
	case "pi-team-preflight":
		return piTeamPreflightCommand(args[1:])
	case "validate":
		return validateCommand(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// teamPresetCommand resolves one package-provided, direct-thread preset. It
// is deliberately read-only: callers persist the returned snapshot in their
// creation receipt and replay that snapshot rather than consulting a later
// package generation.
func teamPresetCommand(args []string) error {
	flags := flag.NewFlagSet("team-preset", flag.ContinueOnError)
	packageRoot := flags.String("package-root", "", "absolute installed package root")
	workspace := flags.String("workspace", "", "registered workspace root")
	provider := flags.String("provider", "", "explicit creation provider")
	id := flags.String("team", "", "package team identifier")
	model := flags.String("model", "", "explicit lead Codex model")
	effort := flags.String("effort", "", "explicit lead reasoning effort")
	socket := flags.String("socket", "", "Codex App Server Unix socket for lead override validation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !canonicalAbsolutePath(*packageRoot) || *id == "" {
		return errors.New("team-preset requires --package-root and --team")
	}
	if (*model == "") != (*effort == "") {
		return errors.New("team-preset requires --model and --effort together")
	}
	if *model != "" && !canonicalAbsolutePath(*socket) {
		return errors.New("team-preset lead override requires --socket")
	}
	installed, err := agentteams.LoadInstalled(*packageRoot)
	if err != nil {
		return err
	}
	if !installed.Managed || installed.Catalog == nil {
		return errors.New("this workspace package has no direct team catalog")
	}
	preset, err := teamruntime.FindCatalogPreset(*installed.Catalog, *id)
	if err != nil {
		return err
	}
	if *model != "" {
		client := newCodexClient(*socket, *workspace)
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		models, err := client.ModelsForDirectory(ctx, "", *packageRoot, *provider)
		if err != nil {
			return fmt.Errorf("load live Codex models: %w", err)
		}
		settings, err := workspacecodex.ResolveNewThreadSettings(models, codex.ThreadSettings{
			Model: *model, ReasoningEffort: *effort,
		})
		if err != nil {
			return err
		}
		preset.LeadModel, preset.LeadEffort = settings.Model, settings.ReasoningEffort
	}
	if err := preset.ValidateCreationSize(); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(preset)
}

type serveOptions struct {
	piHostStateRoot                                          string
	piAuthDirectory                                          string
	unixSocket, workspace, baseURL, devSession, authorityDir string
	userStateRoot                                            string
	packageRoot, workspaceName, registrationMarker           string
	displayLabel, hostLabel, sshHost                         string
	hostProfile, transitionLock                              string
	codexSocket, codexVersion                                string
	gh, tmux                                                 string
	trustedOrigins                                           trustedOriginValues
	clusterProviders                                         clusterProviderValues
}

var clusterProviderName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type clusterProviderValues []cluster.Provider

func (values *clusterProviderValues) String() string { return "" }

func (values *clusterProviderValues) Set(value string) error {
	parts := strings.SplitN(value, "=", 3)
	if len(parts) != 3 || !clusterProviderName.MatchString(parts[0]) || parts[1] == "" ||
		!filepath.IsAbs(parts[2]) {
		return errors.New("cluster provider must be ID=LABEL=/absolute/helper")
	}
	for _, existing := range *values {
		if existing.Name == parts[0] {
			return fmt.Errorf("duplicate cluster provider %q", parts[0])
		}
	}
	*values = append(*values, cluster.Provider{Name: parts[0], Label: parts[1], Helper: parts[2]})
	return nil
}

// trustedOriginValues keeps the service boundary explicit: workspace-host
// supplies only the complete origins of aliases already recorded in its private
// registry. The web package validates every value before use.
type trustedOriginValues []string

func (values *trustedOriginValues) String() string { return strings.Join(*values, ",") }

func (values *trustedOriginValues) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func newServeFlagSet() (*flag.FlagSet, *serveOptions) {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	options := &serveOptions{}
	flags.StringVar(&options.unixSocket, "unix-socket", "", "required HTTP Unix socket")
	flags.StringVar(&options.workspace, "workspace", "", "required workspace root")
	flags.StringVar(&options.baseURL, "base-url", "", "required external base URL")
	flags.Var(&options.trustedOrigins, "trusted-origin", "registered alias HTTPS origin (repeatable)")
	flags.StringVar(&options.displayLabel, "display-label", "Development workspace", "workspace label shown in the portal")
	flags.StringVar(&options.hostLabel, "host-label", "this host", "host label shown in local commands")
	flags.StringVar(&options.sshHost, "ssh-host", "", "optional SSH host for remote attach commands")
	flags.StringVar(&options.devSession, "dev-session", "", "absolute installed dev-session command")
	flags.StringVar(&options.authorityDir, "authority-dir", "", "host-only runtime session authority directory")
	flags.StringVar(&options.userStateRoot, "user-state-root", "", "absolute package-selected user state root")
	flags.StringVar(&options.piAuthDirectory, "pi-auth-dir", "", "optional host-selected shared Pi credential directory")
	flags.StringVar(&options.piHostStateRoot, "pi-host-state-root", "", "optional host-selected Pi launcher state root")
	flags.StringVar(&options.packageRoot, "package-root", "", "absolute current workspace package root")
	flags.StringVar(&options.workspaceName, "workspace-name", "", "registered workspace name")
	flags.StringVar(&options.registrationMarker, "registration-marker", "", "private host registration marker")
	flags.StringVar(&options.hostProfile, "host-profile", "", "selected workspace host profile")
	flags.StringVar(&options.transitionLock, "transition-lock", "", "host-wide runtime transition lock")
	flags.StringVar(&options.codexSocket, "codex-socket", codex.DefaultSocket(), "Codex App Server Unix socket")
	flags.StringVar(&options.codexVersion, "codex-version", "", "Codex client version used by attached terminal sessions")
	flags.StringVar(&options.gh, "gh", "gh", "GitHub CLI executable")
	flags.StringVar(&options.tmux, "tmux", "tmux", "tmux executable")
	flags.Var(&options.clusterProviders, "cluster-provider", "ID=LABEL=/absolute/helper (repeatable)")
	return flags, options
}

func serve(args []string) error {
	flags, options := newServeFlagSet()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("serve accepts no positional arguments")
	}
	if options.userStateRoot == "" || options.packageRoot == "" || options.workspaceName == "" || options.registrationMarker == "" {
		return errors.New("serve requires --user-state-root, --package-root, --workspace-name and --registration-marker")
	}
	if !workspaceNamePattern.MatchString(options.workspaceName) {
		return errors.New("serve workspace name is invalid")
	}
	for flagName, value := range map[string]string{
		"--user-state-root": options.userStateRoot, "--package-root": options.packageRoot,
		"--registration-marker": options.registrationMarker,
	} {
		if value != "" && (!filepath.IsAbs(value) || filepath.Clean(value) != value) {
			return fmt.Errorf("%s must be absolute and canonical", flagName)
		}
	}
	piProviderConfig, piProviderError := loadServePiProviderConfiguration(options.packageRoot)
	logger := log.New(os.Stderr, "workspace-portal: ", log.LstdFlags|log.LUTC)
	stopContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	naming := startNamingRuntime(options.packageRoot, options.unixSocket, options.workspaceName, options.codexVersion, logger)
	closeNaming := sync.OnceFunc(func() { closePortalResource(logger, "naming", naming.Close) })
	defer closeNaming()
	var namingSocket string
	namingCatalog := filepath.Join(options.packageRoot, "share/workspace-portal/codex-models.json")
	if naming != nil {
		namingSocket, namingCatalog = naming.socket, naming.catalog
	}
	history := codex.NewHistoryPool(options.codexSocket)
	closeHistory := sync.OnceFunc(func() { closePortalResource(logger, "history", history.Close) })
	defer closeHistory()
	codexClient := newCodexClientWithHistory(options.codexSocket, options.workspace, history, options.userStateRoot)
	codexClient.HostProfile = options.hostProfile
	if installed, loadErr := agentteams.LoadInstalled(options.packageRoot); loadErr == nil {
		codexClient.RetryCatalog = installed.Catalog
	}
	closeClient := sync.OnceFunc(func() { closePortalResource(logger, "client", codexClient.Close) })
	defer closeClient()
	application, err := portalweb.NewContext(stopContext, portalweb.Config{
		ObserveActivity: true,
		HistoryReader:   history,
		CollectUploads:  true,
		Workspace:       options.workspace, BaseURL: options.baseURL, DisplayLabel: options.displayLabel,
		TrustedOrigins: options.trustedOrigins,
		HostLabel:      options.hostLabel, SSHHost: options.sshHost, DevSession: options.devSession,
		HostProfile: options.hostProfile, GH: options.gh, Tmux: options.tmux, AuthorityDir: options.authorityDir,
		TransitionLock: options.transitionLock, UserStateRoot: options.userStateRoot,
		PiAuthDirectory:  options.piAuthDirectory,
		PiProviderConfig: piProviderConfig, PiProviderConfigError: piProviderError,
		PiHostStateRoot: options.piHostStateRoot,
		RecoverSessions: true,
		PackageRoot:     options.packageRoot, WorkspaceName: options.workspaceName, RegistrationMarker: options.registrationMarker,
		CodexSocket: options.codexSocket, CodexVersion: options.codexVersion,
		NamingSocket: namingSocket, NamingModelCatalogFile: namingCatalog,
		ClusterProviders: options.clusterProviders,
		Logger:           logger, Codex: codexClient,
	})
	if err != nil {
		if stopContext.Err() != nil && errors.Is(err, stopContext.Err()) {
			return nil
		}
		return err
	}
	defer application.Close()
	listener, err := portalListener(options.unixSocket)
	if err != nil {
		return err
	}
	defer listener.Close()
	return servePortalHTTP(stopContext, application, listener, logger, options.baseURL)
}

type portalHTTPApplication interface {
	Handler() http.Handler
	Close()
}

func closePortalResource(logger *log.Logger, phase string, close func()) {
	started := time.Now()
	close()
	logger.Printf("portal_shutdown phase=%s duration_ms=%.3f outcome=complete count=1", phase, float64(time.Since(started))/float64(time.Millisecond))
}

// All HTTP exit paths close application workers before serve's owned-child
// defer runs. Constructor/listener errors use the same outer defer ordering.
func servePortalHTTP(stopContext context.Context, application portalHTTPApplication, listener net.Listener, logger *log.Logger, baseURL string) error {
	defer application.Close()
	// Shutdown waits for handlers but does not cancel their contexts. Ordinary
	// reads and resolver/subscription setup must stop with this serving lifetime.
	// Admitted sends retain their separate shutdown/receipt ownership.
	requestContext, cancelRequests := context.WithCancel(stopContext)
	defer cancelRequests()
	httpServer := &http.Server{
		Handler: application.Handler(), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 * 1024,
		BaseContext: func(net.Listener) context.Context { return requestContext },
	}
	serveDone := make(chan struct{})
	shutdownDone := make(chan error, 1)
	go func() {
		select {
		case <-stopContext.Done():
		case <-serveDone:
		}
		cancelRequests()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 130*time.Second)
		defer cancel()
		httpShutdown := make(chan error, 1)
		go func() {
			started := time.Now()
			err := httpServer.Shutdown(shutdownContext)
			outcome := "complete"
			if err != nil {
				outcome = "error"
			}
			logger.Printf("portal_shutdown phase=http_drain duration_ms=%.3f outcome=%s count=1", float64(time.Since(started))/float64(time.Millisecond), outcome)
			httpShutdown <- err
		}()
		application.Close()
		shutdownDone <- <-httpShutdown
	}()
	logger.Printf("listening on %s for %s", listener.Addr(), baseURL)
	err := httpServer.Serve(listener)
	close(serveDone)
	shutdownErr := <-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return shutdownErr
	}
	return errors.Join(err, shutdownErr)
}

func portalListener(socketPath string) (net.Listener, error) {
	if socketPath == "" {
		return nil, errors.New("--unix-socket is required")
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("Unix socket path already exists and is not a socket: %s", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("remove stale Unix socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect Unix socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o660); err != nil {
		listener.Close()
		return nil, fmt.Errorf("set Unix socket permissions: %w", err)
	}
	return listener, nil
}

type scopedModelCatalog func(context.Context) ([]codex.Model, error)

func (f scopedModelCatalog) ListModels(ctx context.Context) ([]codex.Model, error) { return f(ctx) }

type teamModelCatalog interface {
	ListModels(context.Context) ([]codex.Model, error)
}

// resolveTeamMemberSettings keeps add/configure from falling through to the
// App Server default model. Existing members can only be changed to an exact
// live model/effort pair, matching browser-side validation.
func resolveTeamMemberSettings(ctx context.Context, client teamModelCatalog, command, model, effort string) (codex.ThreadSettings, error) {
	if command != "add" && command != "configure" {
		return codex.ThreadSettings{Model: model, ReasoningEffort: effort}, nil
	}
	if command == "add" && model == "" && effort == "" {
		return codex.ThreadSettings{}, nil
	}
	if model == "" || effort == "" {
		return codex.ThreadSettings{}, fmt.Errorf("team %s requires --model and --effort", command)
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		return codex.ThreadSettings{}, fmt.Errorf("load live Codex models: %w", err)
	}
	settings, err := workspacecodex.ResolveNewThreadSettings(models, codex.ThreadSettings{
		Model: model, ReasoningEffort: effort,
	})
	if err != nil {
		return codex.ThreadSettings{}, err
	}
	return settings, nil
}

// teamCommand is the CLI counterpart of the portal's team endpoint. Both use
// the same private roster and direct App Server thread operations.
func teamCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: workspace-portal team list|preset|apply-preset|add|configure|update-access|remove|assign|require-idle|require-execution-idle|require-archive-ready|require-archive-identities|require-archived|archive|retire|revive|fork")
	}
	command := args[0]
	flags := flag.NewFlagSet("team "+command, flag.ContinueOnError)
	stateRoot := flags.String("user-state-root", "", "private user state root")
	hostProfile := flags.String("host-profile", "", "selected workspace user profile")
	packageRoot := flags.String("package-root", "", "installed workspace package root")
	workspace := flags.String("workspace", "", "development workspace root")
	slug := flags.String("session-slug", "", "development session slug")
	rootThread := flags.String("root-thread-id", "", "lead Codex thread id")
	portalSocket := flags.String("portal-socket", "", "registered private portal socket")
	portalOrigin := flags.String("portal-origin", "", "registered portal origin")
	sourceSlug := flags.String("source-session-slug", "", "source development session slug")
	sourceRootThread := flags.String("source-root-thread-id", "", "source lead Codex thread id")
	socket := flags.String("socket", codex.DefaultSocket(), "Codex App Server Unix socket")
	codexHome := flags.String("codex-home", "", "host-selected Codex home")
	retainedOnly := flags.Bool("retained-only", false, "complete only retained member archival")
	deletionOperationID := flags.String("deletion-operation-id", "", "owning session deletion operation")
	checkOnly := flags.Bool("check-only", false, "validate deletion preparation without publishing")
	authorityDir := flags.String("authority-dir", "", "host-only runtime authority directory")
	cwd := flags.String("cwd", "", "session working directory")
	preset := flags.String("preset", "", "team preset")
	presetFile := flags.String("preset-file", "", "immutable team preset JSON file")
	role := flags.String("role", "", "member role")
	address := flags.String("address", "", "team member address")
	access := flags.String("access", "", "retained member access")
	from := flags.String("from", "lead", "sender address")
	expectedMemberThread := flags.String("expected-member-thread-id", "", "native report caller thread")
	to := flags.String("to", "", "recipient address")
	message := flags.String("message", "", "assignment message")
	messageID := flags.String("message-id", "", "stable assignment retry ID")
	inputFile := flags.String("input-file", "", "file containing an assignment message")
	model := flags.String("model", "", "Codex model")
	effort := flags.String("effort", "", "Codex reasoning effort")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *stateRoot == "" || *workspace == "" || !session.ValidSlug(*slug) {
		return errors.New("team command requires --user-state-root, --workspace, --session-slug and --root-thread-id")
	}
	binding, bindingErr := (session.BackendBindingStore{Root: *stateRoot, Workspace: *workspace}).Load(*slug)
	if bindingErr == nil && binding.Backend == agentbackend.Pi {

		unsupported := false
		flags.Visit(func(value *flag.Flag) {
			switch value.Name {
			case "user-state-root", "workspace", "session-slug", "authority-dir", "portal-socket", "portal-origin", "from":
			case "message-id":
				unsupported = unsupported || (command != "assign" && command != "add")
			case "address":
				unsupported = unsupported || command != "remove"
			case "role", "model", "effort":
				unsupported = unsupported || command != "add"
			case "to", "message", "input-file":
				unsupported = unsupported || command != "assign"
			default:
				unsupported = true
			}
		})
		if unsupported {
			return piruntime.ErrScope
		}
		if *rootThread != "" || *expectedMemberThread != "" || (command != "add" && (*model != "" || *effort != "")) {
			return piruntime.ErrScope
		}
		if *inputFile != "" {
			contents, err := os.ReadFile(*inputFile)
			if err != nil {
				return err
			}
			*message = string(contents)
		}
		return runPiTeamCommand(context.Background(), piTeamCommandOptions{StateRoot: *stateRoot, AuthorityDir: *authorityDir, Workspace: *workspace, Slug: *slug, Socket: *portalSocket, Origin: *portalOrigin, Command: command, Address: *address, From: *from, To: *to, Message: *message, MessageID: *messageID, Role: *role, Model: *model, Effort: *effort}, os.Stdout)
	}
	if bindingErr != nil && !errors.Is(bindingErr, agentbackend.ErrNotBound) {
		return bindingErr
	}
	if *rootThread == "" {
		return errors.New("team command requires --root-thread-id for Codex")
	}
	if *retainedOnly && command != "archive" {
		return errors.New("--retained-only is only valid for team archive")
	}
	if command != "prepare-delete" && (*deletionOperationID != "" || *checkOnly) {
		return errors.New("deletion preparation flags are only valid for team prepare-delete")
	}
	if command == "prepare-delete" && !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(*deletionOperationID) {
		return errors.New("team prepare-delete requires --deletion-operation-id")
	}
	if command == "assign" || command == "add" || command == "configure" || command == "apply-preset" || command == "fork" || command == "revive" {
		if err := maintenance.RequireAdmission(*stateRoot, *hostProfile, *workspace); err != nil {
			return err
		}
	}
	store, err := teamruntime.NewStore(*stateRoot, *workspace)
	if err != nil {
		return err
	}
	if command == "list" {
		presets := teamruntime.Presets()
		executionAccess := ""
		if *packageRoot != "" {
			if !canonicalAbsolutePath(*packageRoot) {
				return errors.New("team list requires a canonical absolute package root")
			}
			installed, err := agentteams.LoadInstalled(*packageRoot)
			if err != nil {
				return err
			}
			if installed.Catalog.FullExecutionAccess() {
				executionAccess = "full_access"
			}
			if installed.Managed && installed.Catalog != nil {
				presets, err = teamruntime.PresetsFromCatalog(*installed.Catalog)
				if err != nil {
					return err
				}
			}
		}
		roster, err := store.Load(*slug, *rootThread)
		if errors.Is(err, os.ErrNotExist) {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"roster": nil, "presets": presets, "executionAccess": executionAccess})
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"roster": roster, "presets": presets, "executionAccess": executionAccess})
	}
	if *cwd == "" {
		*cwd = filepath.Join(*workspace, "work", *slug)
	}
	var teamCatalog *agentteams.Catalog
	if *packageRoot != "" {
		if !canonicalAbsolutePath(*packageRoot) {
			return errors.New("team command requires a canonical absolute package root")
		}
		installed, loadErr := agentteams.LoadInstalled(*packageRoot)
		if loadErr != nil {
			return loadErr
		}
		if installed.Managed {
			if installed.Catalog == nil {
				return errors.New("installed team catalog is unavailable")
			}
			teamCatalog = installed.Catalog
		}
	}
	construct := newCodexClient
	if command == "require-execution-idle" {
		construct = newTransitionClient
	}
	client := construct(*socket, *workspace, *stateRoot)
	client.RetryCatalog = teamCatalog
	client.HostProfile = *hostProfile
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	catalog := scopedModelCatalog(func(ctx context.Context) ([]codex.Model, error) {
		return client.ModelsForDirectory(ctx, *cwd, *hostProfile, "")
	})
	settings, err := resolveTeamMemberSettings(ctx, catalog, command, *model, *effort)
	if err != nil {
		return err
	}
	if command == "add" || command == "configure" {
		*model, *effort = settings.Model, settings.ReasoningEffort
	}
	environment := map[string]string{
		"DEV_SESSION_SLUG": *slug, "DEV_SESSION_WORKSPACE": *workspace, "DEV_SESSION_WORK_DIR": *cwd,
		"DEV_SESSION_REQUIRE_RUNTIME": "1", "DEV_SESSION_CODEX_SOCKET": *socket,
	}
	service := teamruntime.Service{Progress: creationprogress.FromEnvironment(), Store: store, ExpectedReporterThread: *expectedMemberThread, Client: client, Workspace: *workspace, HostProfile: *hostProfile, Catalog: teamCatalog,
		ValidateSettings: func(ctx context.Context, settings codex.ThreadSettings) error {
			models, err := catalog.ListModels(ctx)
			if err != nil {
				return fmt.Errorf("load live Codex models: %w", err)
			}
			_, err = workspacecodex.ResolveNewThreadSettings(models, settings)
			return err
		}}
	var presetSpec teamruntime.Preset
	if command == "apply-preset" {
		if *presetFile == "" {
			return errors.New("team apply-preset requires --preset-file")
		}
		info, statErr := os.Lstat(*presetFile)
		if statErr != nil {
			return fmt.Errorf("inspect team preset: %w", statErr)
		}
		if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 {
			return errors.New("team preset must be a private regular file")
		}
		file, openErr := os.Open(*presetFile)
		if openErr != nil {
			return openErr
		}
		decoder := json.NewDecoder(io.LimitReader(file, 64*1024+1))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&presetSpec)
		if decodeErr == nil && decoder.Decode(&struct{}{}) != io.EOF {
			decodeErr = errors.New("team preset has trailing data")
		}
		if closeErr := file.Close(); decodeErr == nil {
			decodeErr = closeErr
		}
		if decodeErr != nil {
			return fmt.Errorf("decode team preset: %w", decodeErr)
		}
	}
	if *inputFile != "" {
		contents, readErr := os.ReadFile(*inputFile)
		if readErr != nil {
			return readErr
		}
		*message = string(contents)
	}
	var lifecycleSelection []teamruntime.LifecycleSelection
	switch command {
	case "archive", "archive-explicit", "retire", "revive", "prepare-delete", "require-archived", "require-archive-ready", "require-archive-identities":
		nativeCommand := "team/" + command
		if command == "archive" && *retainedOnly {
			nativeCommand = "team/archive-retained"
		}
		selection, parent, admissionErr := admitLifecycleCommand(*workspace, *stateRoot, *authorityDir, *slug, *socket, nativeCommand, nil)
		if admissionErr != nil {
			return admissionErr
		}
		if parent != nil {
			defer parent.Close()
		}
		if selection != nil {
			if command == "prepare-delete" && selection.Operation != "delete" {
				return errors.New("deletion preparation has another lifecycle owner")
			}
			lifecycleSelection = []teamruntime.LifecycleSelection{*selection}
		}
	}
	var result any
	switch command {
	case "preset":
		result, err = service.ApplyPreset(ctx, *slug, *rootThread, *cwd, environment, *preset, *model, *effort)
	case "apply-preset":
		result, err = service.ApplyPresetSpec(ctx, *slug, *rootThread, *cwd, environment, presetSpec)
	case "add":
		result, err = service.Add(ctx, *slug, *rootThread, *cwd, environment, *role, *model, *effort)
	case "configure":
		result, err = service.Configure(ctx, *slug, *rootThread, *address, *model, *effort)
	case "update-access":
		result, err = service.UpdateAccess(ctx, *slug, *rootThread, *address, *access)
	case "remove":
		err = service.Remove(ctx, *slug, *rootThread, *address)
	case "assign":
		result, err = service.Assign(ctx, *slug, *rootThread, *from, *to, *message, *model, *effort, *messageID)
	case "require-execution-idle":
		err = service.RequireExecutionIdleAll(ctx, *slug, *rootThread)
	case "require-idle":
		err = service.RequireIdleAll(ctx, *slug, *rootThread)
	case "prepare-delete":
		err = service.PrepareDelete(ctx, *slug, *rootThread, *deletionOperationID, *checkOnly, lifecycleSelection...)
	case "require-archive-ready", "require-archive-identities":
		if !canonicalAbsolutePath(client.CodexHome) || *codexHome != client.CodexHome ||
			!canonicalAbsolutePath(*socket) || !canonicalAbsolutePath(*authorityDir) ||
			*cwd != filepath.Join(*workspace, "work", *slug) {
			return errors.New("session archive preflight requires deployed Codex and session authority")
		}
		if command == "require-archive-identities" {
			err = service.RequireArchiveIdentitiesAll(ctx, *slug, *rootThread, lifecycleSelection...)
		} else {
			err = service.RequireArchiveReadyAll(ctx, *slug, *rootThread, lifecycleSelection...)
		}
	case "require-archived":
		if !canonicalAbsolutePath(client.CodexHome) || *codexHome != client.CodexHome ||
			!canonicalAbsolutePath(*socket) ||
			!canonicalAbsolutePath(*authorityDir) || *cwd != filepath.Join(*workspace, "work", *slug) {
			return errors.New("team archive preflight requires deployed Codex and session authority")
		}
		err = service.RequireArchivedAll(ctx, *slug, *rootThread, lifecycleSelection...)
	case "archive":
		if *retainedOnly {
			if !canonicalAbsolutePath(client.CodexHome) || *codexHome != client.CodexHome ||
				!canonicalAbsolutePath(*socket) || !canonicalAbsolutePath(*authorityDir) ||
				*cwd != filepath.Join(*workspace, "work", *slug) {
				return errors.New("retained team archive requires deployed Codex and session authority")
			}
			err = service.ArchiveRetainedAll(ctx, *slug, *rootThread, lifecycleSelection...)
		} else {
			err = service.ArchiveAll(ctx, *slug, *rootThread, lifecycleSelection...)
		}
	case "archive-explicit":
		err = service.ArchiveExplicitAll(ctx, *slug, *rootThread, lifecycleSelection...)
	case "retire":
		err = service.RetireAll(ctx, *slug, *rootThread, lifecycleSelection...)
	case "revive":
		err = service.ReviveAll(ctx, *slug, *rootThread, lifecycleSelection...)
	case "fork":
		if !session.ValidSlug(*sourceSlug) || *sourceRootThread == "" {
			return errors.New("team fork requires --source-session-slug and --source-root-thread-id")
		}
		var capture *teamruntime.SourceFork
		path := filepath.Join(*workspace, "worktrees", ".locks", *slug+".fork.json")
		if info, statErr := os.Lstat(path); statErr == nil {
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 64<<10 {
				return errors.New("team fork journal is not private and bounded")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var journal map[string]json.RawMessage
			if err := monitoroperation.DecodeMixedPiRecord(data, &journal, 64<<10); err != nil {
				return err
			}
			if raw, ok := journal["source_fork"]; ok {
				var accepted teamruntime.SourceFork
				if err := json.Unmarshal(raw, &accepted); err != nil {
					return err
				}
				capture = &accepted
				parent, err := inheritedForkParentLock(*workspace, *authorityDir, *slug)
				if err != nil {
					return err
				}
				defer parent.Close()
				owner, _, err := readBoundRootForkJournal(path)
				if err != nil || owner == nil || owner.RootFork.State != "known" || owner.RootFork.ThreadID != *rootThread {
					return errors.New("team fork root owner changed")
				}
				var savedSlug, savedSource, savedRoot, savedState, identity string
				if json.Unmarshal(journal["slug"], &savedSlug) != nil || json.Unmarshal(journal["source_slug"], &savedSource) != nil || json.Unmarshal(journal["source_thread_id"], &savedRoot) != nil || json.Unmarshal(journal["state"], &savedState) != nil || json.Unmarshal(journal["tmux_identity"], &identity) != nil || savedSlug != *slug || savedSource != *sourceSlug || savedRoot != *sourceRootThread || savedState != "creating" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(identity) {
					return errors.New("team fork arguments differ from accepted source")
				}
				selected, err := filepath.EvalSymlinks(*hostProfile)
				if err != nil || selected != *packageRoot {
					return errors.New("team fork selected package changed")
				}
				contractBytes, err := os.ReadFile(filepath.Join(selected, "share", "workspace-portal", "runtime-contract.json"))
				var contract map[string]json.RawMessage
				if err != nil || session.DecodePiCreationJSON(contractBytes, &contract, 64<<10) != nil || string(contract["codexProviderBindingStateSchema"]) != "3" || string(contract["codexProviderForkTransitionPolicy"]) != "1" || string(contract["codexRuntimeAuthoritySchema"]) != "3" {
					return errors.New("team fork binding consumers are unavailable")
				}
				catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
				if err != nil {
					return err
				}
				state, err := codexproviders.ReadBindingState(*stateRoot, filepath.Base(filepath.Dir(*socket)), *workspace, *socket, catalog)
				if err != nil {
					return err
				}
				binding, ok := state.Bindings[*sourceSlug]
				if !ok || binding.RootThreadID != *sourceRootThread || binding.Incarnation != capture.Incarnation || state.BlocksSession(*sourceSlug, *sourceRootThread) || state.BlocksSession(*slug, *rootThread) {
					return errors.New("team fork original source/state is changed or fenced")
				}
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		source, loadErr := store.Load(*sourceSlug, *sourceRootThread)
		if errors.Is(loadErr, os.ErrNotExist) {
			if capture != nil {
				if capture.RosterDigest == nil && len(capture.Members) == 0 {
					release, err := store.TryLockOperation(*slug)
					if err != nil {
						return err
					}
					defer release()
					if _, err := store.Load(*slug, *rootThread); !errors.Is(err, os.ErrNotExist) {
						return errors.New("accepted absent fork roster has an unexpected destination")
					}
					result = map[string]any{"roster": nil}
				} else {
					result, err = service.Fork(ctx, nil, *slug, *rootThread, *cwd, environment, capture)
				}
			} else {
				result = map[string]any{"roster": nil}
			}
		} else if loadErr != nil {
			err = loadErr
		} else if capture != nil {
			result, err = service.Fork(ctx, source, *slug, *rootThread, *cwd, environment, capture)
		} else {
			result, err = service.Fork(ctx, source, *slug, *rootThread, *cwd, environment)
		}

	default:
		return fmt.Errorf("unknown team command %q", command)
	}
	if err != nil {
		return err
	}
	if result == nil {
		roster, loadErr := store.Load(*slug, *rootThread)
		if (command == "require-archive-ready" || command == "require-archive-identities" || command == "require-archived" || command == "archive" && *retainedOnly) && errors.Is(loadErr, os.ErrNotExist) {
			result = map[string]any{"roster": nil}
		} else if loadErr != nil {
			return loadErr
		} else {
			result = roster
		}
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func threadCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: workspace-portal thread create|fork|set-name|models|resolve-fork-settings|ensure-initial|observe|activity|require-materialized|require-idle|require-execution-idle|require-archived|retire")
	}
	command := args[0]
	flags := flag.NewFlagSet("thread "+command, flag.ContinueOnError)
	userStateRoot := flags.String("user-state-root", "", "package-selected user state root")
	socket := flags.String("socket", codex.DefaultSocket(), "Codex App Server Unix socket")
	codexHome := flags.String("codex-home", "", "host-selected Codex home")
	cwd := flags.String("cwd", "", "thread working directory")
	workspace := flags.String("workspace", "", "development workspace root")
	sessionSlug := flags.String("session-slug", "", "development session slug")
	worktreesDir := flags.String("worktrees-dir", "", "development session worktree directory")
	portalBaseURL := flags.String("portal-base-url", "", "workspace portal base URL")
	portalURL := flags.String("portal-url", "", "development session portal URL")
	portalCommand := flags.String("portal-command", "", "stable workspace-portal executable")
	authorityDir := flags.String("authority-dir", "", "host-only runtime authority directory")
	tmuxSocket := flags.String("tmux-socket", "", "dedicated tmux socket")
	codexCommand := flags.String("codex-command", "", "absolute Codex executable")
	codexVersion := flags.String("codex-version", "", "Codex client version")
	name := flags.String("name", "", "thread name")
	threadID := flags.String("thread-id", "", "thread id")
	model := flags.String("model", "", "Codex model")
	effort := flags.String("effort", "", "Codex reasoning effort")
	inputFile := flags.String("input-file", "", "file containing a message")
	leadInstructions := flags.String("lead-instructions", "", "frozen lead role instructions")
	requireRuntime := flags.Bool("require-runtime", false, "require complete deployed runtime provenance")
	fresh := flags.Bool("fresh", false, "direct fork for a locked first invocation")
	provider := flags.String("provider", "", "explicit destination provider for fork settings")
	recoverCreating := flags.Bool("recover-creating", false, "reconcile a creating thread by working directory")
	recoverArchived := flags.Bool("recover-archived", false, "restore the exact thread retained by revived tracking")
	startUnmaterialized := flags.Bool("start-unmaterialized", false, "allow the first turn on a proven fresh thread")
	force := flags.Bool("force", false, "interrupt an active thread before retiring it")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if command == "create" || command == "fork" || command == "ensure-initial" {
		if err := maintenance.RequireAdmission(*userStateRoot, filepath.Join(*userStateRoot, "profile"), *workspace); err != nil {
			return err
		}
	}
	construct := newCodexClient
	if command == "require-execution-idle" {
		construct = newTransitionClient
	}
	client := construct(*socket, *workspace, *userStateRoot)
	defer client.Close()
	// Directory discovery can repair a large persisted history. Keep ordinary
	// metadata commands short, but allow recovery to complete that scan.
	timeout := time.Minute
	if command == "create" || command == "fork" || command == "retire" || command == "quiesce-explicit" {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	runtime := threadRuntime{
		Slug: *sessionSlug, Workspace: *workspace, WorkDir: *cwd,
		WorktreesDir: *worktreesDir, PortalBaseURL: *portalBaseURL, PortalURL: *portalURL,
		AuthorityDir: *authorityDir, TmuxSocket: *tmuxSocket,
		CodexCommand: *codexCommand, CodexSocket: *socket, CodexVersion: *codexVersion,
		PortalCommand: *portalCommand,
	}
	if (command == "retire" || command == "quiesce-explicit") && *workspace != "" || command == "create" && *recoverArchived && os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") == "revive" {
		slug := *sessionSlug
		if slug == "" {
			slug = filepath.Base(*cwd)
		}
		if !session.ValidSlug(slug) || *cwd != filepath.Join(*workspace, "work", slug) || flags.NArg() != 0 {
			return errors.New("lifecycle root requires its exact session directory")
		}
		var expectedForce *bool
		if command == "retire" {
			expectedForce = force
		}
		selection, parent, err := admitLifecycleCommand(*workspace, *userStateRoot, *authorityDir, slug, *socket, "thread/"+command, expectedForce)
		if err != nil {
			return err
		}
		if parent != nil {
			defer parent.Close()
		}
		if selection != nil {
			store, err := teamruntime.NewStore(*userStateRoot, *workspace)
			if err != nil {
				return err
			}
			release, err := store.TryLockOperation(slug)
			if err != nil {
				return err
			}
			defer release()
			summary, err := session.Find(*workspace, slug)
			if err != nil {
				return err
			}
			if err := store.ValidateLifecycleSelection(slug, summary.Codex.ThreadID, *selection); err != nil {
				return err
			}
			expectedNative := summary.Codex.ThreadID
			if selection.Targets != nil {
				expectedNative = selection.Targets.ThreadID
			}
			if *threadID != expectedNative {
				return errors.New("lifecycle root native operand differs from saved target")
			}
		}
	}
	switch command {
	case "observe":
		if *threadID == "" || *cwd == "" {
			return errors.New("thread observe requires --thread-id and --cwd")
		}
		observation, err := client.ObserveThread(ctx, *threadID, *cwd)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(observation)
	case "activity":
		if *threadID == "" || *cwd == "" {
			return errors.New("thread activity requires --thread-id and --cwd")
		}
		activities, err := client.ListThreadActivity(ctx, []workspacecodex.ThreadActivity{{ID: *threadID, Cwd: *cwd}})
		if err != nil {
			return err
		}
		if len(activities) != 1 || activities[0].ID != *threadID || activities[0].Cwd != *cwd {
			return errors.New("thread activity returned the wrong conversation")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"threadId": activities[0].ID, "cwd": activities[0].Cwd, "updatedAt": activities[0].UpdatedAt.Unix(),
		})
	case "models":
		models, err := client.ModelsForDirectory(ctx, *cwd, filepath.Dir(filepath.Dir(*portalCommand)), "")
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(models)
	case "resolve-fork-settings":
		if *threadID == "" {
			return errors.New("thread resolve-fork-settings requires --thread-id")
		}
		settings, err := client.ResolveForkSettings(ctx, *threadID, codex.ThreadSettings{
			Model: *model, ReasoningEffort: *effort,
		}, filepath.Dir(filepath.Dir(*portalCommand)), *provider)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"model": settings.Model, "reasoningEffort": settings.ReasoningEffort,
		})
	case "create":
		if !*requireRuntime || !runtime.complete() {
			return errors.New("thread create requires complete workspace, lifecycle, tmux and Codex provenance")
		}
		if *recoverCreating && *recoverArchived {
			return errors.New("thread create cannot combine creating and archived recovery")
		}
		if *recoverArchived && *threadID == "" {
			return errors.New("archived thread recovery requires --thread-id")
		}
		var id string
		var err error
		settings := rootThreadSettings(*model, *effort, runtime.Slug, runtime.Workspace, *leadInstructions)
		resolveSettings := func() (codex.ThreadSettings, error) {
			if settings.Model == "" && settings.ReasoningEffort == "" {
				return settings, nil
			}
			models, listErr := client.ModelsForDirectory(ctx, *cwd, filepath.Dir(filepath.Dir(*portalCommand)), "")
			if listErr != nil {
				return codex.ThreadSettings{}, fmt.Errorf("load Codex models: %w", listErr)
			}
			return workspacecodex.ResolveNewThreadSettings(models, settings)
		}
		var retained *workspacecodex.RetainedRootRecovery
		var retainedStore *teamruntime.Store
		summary, summaryErr := session.Find(*workspace, *sessionSlug)
		if summaryErr != nil && !errors.Is(summaryErr, os.ErrNotExist) {
			return summaryErr
		}
		if summaryErr == nil && summary.Codex.BindingSchema == 3 {
			if *recoverCreating || *threadID == "" || summary.Codex.SocketPath != *socket || *cwd != filepath.Join(*workspace, "work", *sessionSlug) {
				return errors.New("bound retained root requires its exact current recovery target")
			}
			selected, err := filepath.EvalSymlinks(filepath.Join(*userStateRoot, "profile"))
			if err != nil {
				return err
			}
			portal, err := filepath.EvalSymlinks(*portalCommand)
			if err != nil || portal != filepath.Join(selected, "bin", "workspace-portal") {
				return errors.New("retained root recovery package changed")
			}
			data, err := os.ReadFile(filepath.Join(selected, "share", "workspace-portal", "runtime-contract.json"))
			var contract map[string]json.RawMessage
			if err != nil || session.DecodePiCreationJSON(data, &contract, 64<<10) != nil || string(contract["codexProviderBindingStateSchema"]) != "3" || string(contract["codexProviderForkTransitionPolicy"]) != "1" || string(contract["codexRuntimeAuthoritySchema"]) != "3" {
				return errors.New("selected package cannot recover bound roots")
			}
			retainedStore, err = teamruntime.NewStore(*userStateRoot, *workspace)
			if err != nil {
				return err
			}
			current, err := retainedStore.ResolveHistoryConversation(*sessionSlug, summary.Codex.ThreadID, "lead", summary.Codex.ThreadID)
			if err != nil || current.ThreadID != *threadID {
				return errors.New("retained native recovery target changed")
			}
			path := filepath.Join(*workspace, "worktrees", ".locks", *sessionSlug+".start.json")
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 64<<10 {
				return errors.New("retained root recovery has no owned start journal")
			}
			data, err = os.ReadFile(path)
			var journal struct {
				Schema       int    `json:"schema"`
				Slug         string `json:"slug"`
				State        string `json:"state"`
				TmuxIdentity string `json:"tmux_identity"`
			}
			if err != nil || session.DecodePiCreationJSON(data, &journal, 64<<10) != nil || journal.Schema != 1 || journal.Slug != *sessionSlug || journal.State != "creating" || journal.TmuxIdentity != current.Key.Incarnation {
				return errors.New("retained start incarnation changed")
			}
			progress, err := session.PendingLifecycleProgress(*workspace, *sessionSlug)
			if err != nil {
				return err
			}
			if progress != nil && (progress.Operation != "revive" || progress.Phase != "runtime_starting" || !progress.RootRecorded || progress.RetainedThreadID == nil || *progress.RetainedThreadID != summary.Codex.ThreadID) {
				return errors.New("retained lifecycle owner changed")
			}
			retained = &workspacecodex.RetainedRootRecovery{Key: current.Key, NativeThreadID: current.ThreadID, ExpectedRevision: current.Revision, Provider: current.Provider}
		}
		if retained != nil {
			if *recoverArchived {
				id, err = client.RecoverArchivedRetainedRoot(ctx, *retained, runtime.environment(), settings)
			} else {
				id, err = client.ResumeRetainedRoot(ctx, *retained, runtime.environment(), settings)
			}
			if err == nil {
				after, checkErr := retainedStore.ResolveHistoryConversation(*sessionSlug, retained.Key.RootThreadID, "lead", retained.Key.OriginalThreadID)
				if checkErr != nil || after.ThreadID != retained.NativeThreadID || after.Key != retained.Key || after.Revision != retained.ExpectedRevision || after.Provider != retained.Provider {
					return errors.New("retained binding changed during native recovery")
				}
			}
		} else if *recoverArchived {
			id, err = client.RecoverArchivedThread(ctx, *threadID, *cwd, runtime.environment())
			if err == nil {
				err = client.ReconcileThreadInstructionsWithPolicy(ctx, id, settings.Policy)
			}
		} else if *recoverCreating {
			id, err = withCreationRoster(ctx, *userStateRoot, *workspace, *sessionSlug, *threadID,
				func(options workspacecodex.RecoveryOptions) (string, error) {
					return client.RecoverCreatingThreadWithOptions(ctx, *threadID, *cwd, runtime.environment(), settings.Policy, resolveSettings, options)
				})
		} else {
			if *threadID == "" {
				settings, err = resolveSettings()
				if err != nil {
					return err
				}
			}
			id, err = client.OpenThreadWithSettings(ctx, *threadID, *cwd, runtime.environment(), settings)
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"threadId": id})
	case "fork":
		if !*requireRuntime || !runtime.complete() || *threadID == "" {
			return errors.New("thread fork requires a source thread and complete runtime provenance")
		}
		settings := rootThreadSettings(*model, *effort, runtime.Slug, runtime.Workspace, *leadInstructions)
		var id string
		var err error
		boundID, handled, release, boundErr := acceptedRootFork(ctx, client, runtime, *userStateRoot, *threadID, settings)
		if release != nil {
			defer release()
		}
		if handled {
			id, err = boundID, boundErr
		} else if *fresh {
			id, err = client.ForkThread(ctx, *threadID, *cwd, runtime.environment(), settings)
		} else {
			id, err = withCreationRoster(ctx, *userStateRoot, *workspace, *sessionSlug, "",
				func(options workspacecodex.RecoveryOptions) (string, error) {
					return client.RecoverForkThreadWithOptions(ctx, *threadID, *cwd, runtime.environment(), settings, options)
				})
		}
		if err != nil {
			return err
		}
		if *userStateRoot != "" {
			store, err := uploads.ForWorkspace(*workspace, *userStateRoot)
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(store.Directory, "catalog.json")); err == nil {
				hasFiles, err := store.HasThread(ctx, *threadID)
				if err != nil {
					return err
				}
				if hasFiles {
					epoch, err := session.CompletedRemovalHistory(*workspace, *sessionSlug, *userStateRoot)
					if err != nil {
						return err
					}
					target, err := store.SessionScope(ctx, *sessionSlug, id, epoch)
					if err != nil {
						return err
					}
					transcript, err := client.ReadThread(ctx, id)
					if err != nil {
						return err
					}

					if err := store.Fork(ctx, *threadID, target, transcript.Entries); err != nil {
						return err
					}
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"threadId": id})
	case "set-name":
		if *threadID == "" || *name == "" {
			return errors.New("thread set-name requires --thread-id and --name")
		}
		return client.SetName(ctx, *threadID, *name)
	case "ensure-initial":
		if *threadID == "" || *cwd == "" || *inputFile == "" {
			return errors.New("thread ensure-initial requires --thread-id, --cwd and --input-file")
		}
		input, err := os.ReadFile(*inputFile)
		if err != nil {
			return err
		}
		if len(input) == 0 || len(input) > session.MaxMessageBytes {
			return fmt.Errorf(
				"thread input must contain between 1 and %s bytes",
				session.FormattedMaxMessageBytes(),
			)
		}
		message := bytes.TrimSpace(input)
		if len(message) == 0 {
			return errors.New("thread input must not be blank")
		}
		return client.EnsureInitialMessage(ctx, *threadID, *cwd, string(message), *startUnmaterialized)
	case "ensure-settings":
		if !*requireRuntime || !runtime.complete() || *threadID == "" || *model == "" || *effort == "" {
			return errors.New("thread ensure-settings requires frozen settings and complete runtime provenance")
		}
		if err := client.RequireThreadIdle(ctx, *threadID, *cwd); err != nil {
			return err
		}
		actual, err := client.UpdateThreadSettings(ctx, *threadID, codex.ThreadSettingsUpdate{
			Model: model, ReasoningEffort: effort,
		})
		if err != nil {
			return err
		}
		if actual.Model != *model || actual.ReasoningEffort != *effort {
			return errors.New("Codex did not restore the frozen fork settings")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"model": actual.Model, "reasoningEffort": actual.ReasoningEffort,
		})
	case "require-execution-idle":
		if *threadID == "" || !canonicalAbsolutePath(*cwd) || !canonicalAbsolutePath(*workspace) || !canonicalAbsolutePath(*userStateRoot) {
			return errors.New("transition proof requires thread, directory, workspace and selected state root")
		}
		return client.RequireThreadExecutionIdle(ctx, *threadID, *cwd)
	case "require-idle":
		if *threadID == "" || *cwd == "" {
			return errors.New("thread require-idle requires --thread-id and --cwd")
		}
		return client.RequireThreadIdle(ctx, *threadID, *cwd)
	case "require-archived":
		if *threadID == "" || !canonicalAbsolutePath(*cwd) || !canonicalAbsolutePath(*socket) ||
			!canonicalAbsolutePath(client.CodexHome) || *codexHome != client.CodexHome ||
			!canonicalAbsolutePath(*authorityDir) {
			return errors.New("thread archive preflight requires trusted thread, directory and runtime provenance")
		}
		state, err := client.ProveArchivedRootThread(ctx, *threadID, *cwd)
		if err != nil {
			return err
		}
		if state != workspacecodex.ArchiveArchived {
			return errors.New("retained session conversation is not archived")
		}
		return nil
	case "require-materialized":
		if *threadID == "" || *cwd == "" {
			return errors.New("thread require-materialized requires --thread-id and --cwd")
		}
		return client.RequireThreadMaterialized(ctx, *threadID, *cwd)
	case "quiesce-explicit":
		if *threadID == "" || *cwd == "" {
			return errors.New("explicit archive requires thread and directory")
		}
		state, err := client.ProveArchivedRootThread(ctx, *threadID, *cwd)
		if err != nil {
			return err
		}
		if state == workspacecodex.ArchiveArchived {
			return nil
		}
		if err := client.QuiesceExplicitArchive(ctx, *threadID, *cwd); err != nil {
			return err
		}
		return client.QuiesceNativeDescendants(ctx, *threadID, *cwd)
	case "retire":
		if *cwd == "" {
			return errors.New("thread retire requires --cwd")
		}
		return client.RetireThread(ctx, *threadID, *cwd, *force)
	default:
		return fmt.Errorf("unknown thread command %q", command)
	}
}

// The package root is admitted before loading a fixed Pi config path. This
// helper performs no auth/model/runtime operation and ordinary absence disables
// Pi only. A malformed configured value is passed as a Pi-only refusal to web.
func loadServePiProviderConfiguration(packageRoot string) (*piproviders.PackageConfiguration, error) {
	if !filepath.IsAbs(packageRoot) || filepath.Clean(packageRoot) != packageRoot {
		return nil, errors.New("Pi provider package root must be absolute and canonical")
	}
	resolved, err := filepath.EvalSymlinks(packageRoot)
	if err != nil || resolved != packageRoot {
		return nil, errors.New("Pi provider package root must be resolved")
	}
	configuration, err := piproviders.LoadInstalled(packageRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = portalweb.ValidatePiProviderConfiguration(configuration); err != nil {
		return nil, err
	}
	return &configuration, nil
}
func piProvidersCommand(args []string) error {
	if len(args) == 0 || args[0] != "validate" {
		return errors.New("usage: workspace-portal pi-providers validate --package-root ROOT")
	}
	flags := flag.NewFlagSet("pi-providers validate", flag.ContinueOnError)
	packageRoot := flags.String("package-root", "", "admitted immutable workspace package root")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *packageRoot == "" {
		return errors.New("pi-providers validate requires only --package-root ROOT")
	}
	configuration, err := loadServePiProviderConfiguration(*packageRoot)
	if err != nil {
		return err
	}
	if configuration == nil {
		return errors.New("Pi provider configuration is missing")
	}
	fmt.Println("Pi provider configuration is valid")
	return nil
}

func validateCommand(args []string) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	workspace := flags.String("workspace", "", "workspace root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("validate accepts no positional arguments")
	}
	if *workspace == "" {
		return errors.New("validate requires --workspace")
	}
	summaries, err := session.List(*workspace)
	if err != nil {
		return err
	}
	fmt.Printf("validated %d portal manifest(s)\n", len(summaries))
	return nil
}

// uploadCommand is used by the session deletion journal after thread retirement
// and tracking preservation. Repetition completes interrupted file reclamation.
func uploadCommand(args []string) error {
	if len(args) == 0 || args[0] != "remove-session" {
		return errors.New("usage: workspace-portal uploads remove-session")
	}
	flags := flag.NewFlagSet("uploads remove-session", flag.ContinueOnError)
	workspace := flags.String("workspace", "", "workspace root")
	state := flags.String("user-state-root", "", "user state root")
	slug := flags.String("session-slug", "", "session slug")
	thread := flags.String("thread-id", "", "retired thread identity")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if !session.ValidSlug(*slug) || flags.NArg() != 0 {
		return errors.New("session slug and retired thread identity are required")
	}
	store, err := uploads.ForWorkspace(*workspace, *state)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(store.Directory, "catalog.json")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	epoch, err := session.CompletedRemovalHistory(*workspace, *slug, *state)
	if err != nil {
		return err
	}
	return store.RemoveSession(ctx, *slug, *thread, epoch)
}

func captureComparisonCommand(args []string) error {
	flags := flag.NewFlagSet("capture-comparison", flag.ContinueOnError)
	workspace := flags.String("workspace", "", "workspace root")
	stateRoot := flags.String("user-state-root", "", "private state root")
	slug := flags.String("slug", "", "session slug")
	name := flags.String("name", "", "registered repository name")
	base := flags.String("base", "", "exact historical base commit")
	head := flags.String("head", "", "exact historical head commit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *workspace == "" || *slug == "" || *name == "" || (*base == "") != (*head == "") {
		return errors.New("capture-comparison requires --workspace, --slug, --name and, optionally, both --base and --head")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pair, err := portalweb.CaptureRepositoryComparison(ctx, *workspace, *stateRoot, *slug, *name, *base, *head)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(pair)
}

// agentTeamsCommand is the bounded JSON bridge for host registration.
// Filesystem authorities remain explicit flags so callers cannot hide them
// inside policy JSON.
func agentTeamsCommand(args []string) error {
	return agentTeamsCommandIO(args, os.Stdin, os.Stdout)
}

func agentTeamsCommandIO(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: workspace-portal agent-teams registration")
	}
	command := args[0]
	if command != "registration" {
		return fmt.Errorf("unknown agent team command %q", command)
	}
	flags := flag.NewFlagSet("agent-teams "+command, flag.ContinueOnError)
	packageRoot := flags.String("package-root", "", "absolute installed or candidate package root")
	stateRoot := flags.String("state-root", "", "absolute private user-state root")
	workspace := flags.String("workspace", "", "absolute canonical workspace root")
	hostProfile := flags.String("host-profile", "", "stable selected host profile")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("agent team helper accepts no positional arguments")
	}
	if !canonicalAbsolutePath(*packageRoot) || !canonicalAbsolutePath(*stateRoot) || !canonicalAbsolutePath(*workspace) {
		return errors.New("registration requires --package-root, --state-root and --workspace")
	}
	requestData, err := readAgentTeamsRequest(input)
	if err != nil {
		return err
	}
	if _, err := agentteams.DecodeRegistrationRequest(requestData); err != nil {
		return err
	}
	plan, err := agentteams.BuildRegistrationPlan(*packageRoot)
	if err != nil {
		return err
	}
	if *hostProfile == "" {
		*hostProfile = filepath.Join(*stateRoot, "profile")
	}
	plan, err = plan.WithWorkspaceReporting(*packageRoot, *stateRoot, *workspace, *hostProfile)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(plan)
}

func readAgentTeamsRequest(input io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(input, int64(agentteams.MaxHelperRequestBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("read agent team helper request: %w", err)
	}
	if len(data) > agentteams.MaxHelperRequestBytes {
		return nil, errors.New("agent team helper request exceeds bounds")
	}
	return data, nil
}

func canonicalAbsolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

// withCreationRoster binds exclusions to the independently recorded root. A
// roster cannot supply its own root identity when the manifest has lost it.
func withCreationRoster(ctx context.Context, stateRoot, workspace, slug, rootID string,
	run func(workspacecodex.RecoveryOptions) (string, error),
) (string, error) {
	options := workspacecodex.RecoveryOptions{Progress: creationprogress.FromEnvironment()}
	if stateRoot == "" {
		return "", errors.New("creation recovery requires the selected user state root")
	}
	store, err := teamruntime.NewStore(stateRoot, workspace)
	if err != nil {
		return "", err
	}
	release, err := store.LockOperation(ctx, slug)
	if err != nil {
		return "", err
	}
	locked := true
	unlock := func() {
		if locked {
			locked = false
			release()
		}
	}
	defer unlock()
	roster, err := store.Load(slug, rootID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("validate retained creation roster: %w", err)
	}
	options.BeforeCreate = unlock
	if roster != nil {
		if rootID == "" {
			return "", errors.New("retained roster has no independently recorded root")
		}
		options.RetainedRoster = true
		options.ExcludedThreads = map[string]string{}
		for _, member := range roster.Members {
			if member.Thread != "" {
				options.ExcludedThreads[member.Thread] = member.ProjectID
			}
		}
	}
	return run(options)
}
