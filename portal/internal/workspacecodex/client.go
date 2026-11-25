package workspacecodex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/creationprogress"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

const threadSourceKind = "vscode"

// Client keeps development-workspace recovery and retirement policy above the
// reusable App Server client.
type Client struct {
	*codex.Client
	CodexHome            string
	RecoveryRoot         string
	HostProfile          string
	PackageRoot          string
	workspace, socket    string
	submissionLedgerPath string
	transitionProof      bool
	archiveProof         func(context.Context, string, string, string) (ArchiveState, error)
	// Set only by the trusted composition, never browser input. Returning a
	// scope also proves exact root/retained-team ownership for admission.
	RetryPolicyResolver     func(context.Context, string) (codex.ThreadPolicy, error)
	RetryCatalog            *agentteams.Catalog
	retryCommonInstructions string
	deliveryObserver        func(codex.DeliveryEvent)
	retryProofMu            sync.Mutex
	retryProofReaders       map[string]retryProofReader
	retryProofPageBudget    int
}

func NewWithOptions(socket, workspace string, options codex.ClientOptions) *Client {
	return newWithOptions(socket, workspace, options, false)
}

// NewTransitionClient is a cold proof connection, never an observer subscription.
func NewTransitionClient(socket, workspace string, options codex.ClientOptions) *Client {
	options.ObserverOnly = true
	return newWithOptions(socket, workspace, options, true)
}

func newWithOptions(socket, workspace string, options codex.ClientOptions, transition bool) *Client {
	if workspace != "" {
		options.RuntimeWorkspaceRoots = []string{workspace}
	}
	options.ThreadSourceKinds = []string{threadSourceKind}
	options.NonBlockingUserInput = &codex.NonBlockingUserInputPolicy{
		HiddenGrace: 60 * time.Second, VisibleCountdown: 60 * time.Second,
	}
	client := &Client{CodexHome: os.Getenv("DEV_WORKSPACE_CODEX_HOME"), workspace: workspace, socket: socket}
	if executable, err := os.Executable(); err == nil {
		if executable, err = filepath.EvalSymlinks(executable); err == nil {
			client.PackageRoot = filepath.Dir(filepath.Dir(executable))
		}
	}
	client.retryCommonInstructions = options.DeveloperInstructions
	client.deliveryObserver = options.DeliveryObserver
	client.transitionProof = transition
	if transition {
		options.AllowImplicitResume = func(string) bool { return false }
	} else {
		options.AllowImplicitResume = client.allowImplicitResume
	}
	options.LifecycleRequestSettings = client.providerRequestSettings
	client.submissionLedgerPath = options.SubmissionLedgerPath
	client.Client = codex.NewWithOptions(socket, options)
	return client
}

func (c *Client) providerState() (*codexproviders.State, error) {
	name := os.Getenv("DEV_WORKSPACE_NAME")
	if c.RecoveryRoot == "" || name == "" {
		return nil, nil
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if errors.Is(err, os.ErrNotExist) {
		path, pathErr := codexproviders.StatePath(c.RecoveryRoot, name)
		if pathErr != nil {
			return nil, pathErr
		}
		if _, stateErr := os.Lstat(path); errors.Is(stateErr, os.ErrNotExist) {
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	state, err := codexproviders.ReadState(c.RecoveryRoot, name, c.workspace, c.socket, catalog)
	return &state, err
}

type bindingReceiptLineage struct {
	Current codexproviders.ResolvedBinding
	Targets []codexproviders.ReceiptTarget
}

// This is local identity/lineage proof, not execution admission. Owning runtime
// and roster callers still authenticate the original key before a new send.
func (c *Client) bindingReceiptTargets(thread string) (*bindingReceiptLineage, error) {
	name := os.Getenv("DEV_WORKSPACE_NAME")
	if c.RecoveryRoot == "" || name == "" {
		return nil, nil
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			path, pathErr := codexproviders.StatePath(c.RecoveryRoot, name)
			if pathErr != nil {
				return nil, pathErr
			}
			if _, stateErr := os.Lstat(path); errors.Is(stateErr, os.ErrNotExist) {
				return nil, nil
			}
		}
		return nil, err
	}
	state, err := codexproviders.ReadBindingState(c.RecoveryRoot, name, c.workspace, c.socket, catalog)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if state.Schema != 3 {
		return nil, nil
	}
	for slug, binding := range state.Bindings {
		for original, conversation := range binding.Conversations {
			if conversation.ThreadID != thread {
				continue
			}
			summary, err := session.Find(c.workspace, slug)
			if err != nil || summary.Schema != 6 || summary.Codex.BindingSchema != 3 || summary.Codex.ThreadID != binding.RootThreadID || summary.Codex.SocketPath != c.socket {
				return nil, errors.New("receipt binding origin changed")
			}
			authority, err := session.LoadRuntimeAuthority(filepath.Join(filepath.Dir(c.socket), "authority"), slug, c.workspace)
			if err != nil {
				return nil, err
			}
			current, err := authority.ResolveCodexBinding(summary, state, conversation.Member, original, true)
			if err != nil || current.ThreadID != thread {
				return nil, errors.New("receipt current binding changed")
			}
			targets, err := state.ReceiptTargets(current.Key, true, func(operation string) ([]byte, error) {
				return codexproviders.ReadBindingEvidence(c.RecoveryRoot, name, operation)
			})
			if err != nil {
				return nil, err
			}
			return &bindingReceiptLineage{Current: current, Targets: targets}, nil
		}
	}
	// Exact original-target recovery outside the active route retains its own
	// namespace. This function never substitutes another thread for that caller.
	return nil, nil
}

func (c *Client) providerSettings(ctx context.Context, thread string) (codex.ThreadSettings, error) {
	return c.providerRequestSettings(ctx, codex.LifecycleRequest{Method: "thread/resume", ThreadID: thread})
}

func (c *Client) requireCurrentBindingPackage() error {
	selected, err := filepath.EvalSymlinks(c.maintenanceProfile())
	if err != nil || c.PackageRoot == "" || selected != c.PackageRoot {
		return errors.New("current provider package generation changed")
	}
	data, err := os.ReadFile(filepath.Join(selected, "share", "workspace-portal", "runtime-contract.json"))
	var contract map[string]json.RawMessage
	if err != nil || session.DecodePiCreationJSON(data, &contract, 64<<10) != nil || string(contract["codexProviderBindingStateSchema"]) != "3" || string(contract["codexProviderForkTransitionPolicy"]) != "1" || string(contract["codexRuntimeAuthoritySchema"]) != "3" {
		return errors.New("current provider package cannot admit bindings")
	}
	return nil
}

// Scope comes from native metadata; ready authority, R1 and the existing
// policy resolver independently prove that this is the current eligible row.
func (c *Client) markedResumeSettings(ctx context.Context, request codex.LifecycleRequest) (codex.ThreadSettings, bool, error) {
	if request.Method != "thread/resume" || request.ThreadID == "" {
		return codex.ThreadSettings{}, false, nil
	}
	lineage, err := c.bindingReceiptTargets(request.ThreadID)
	if err != nil || lineage == nil {
		return codex.ThreadSettings{}, false, err
	}
	metadata, err := c.ReadThreadMetadata(ctx, request.ThreadID, false)
	if err != nil {
		return codex.ThreadSettings{}, false, err
	}
	slug := filepath.Base(metadata.Cwd)
	if !session.ValidSlug(slug) || metadata.Cwd != filepath.Join(c.workspace, "work", slug) {
		return codex.ThreadSettings{}, false, errors.New("provider destination is outside its session")
	}
	summary, err := session.Find(c.workspace, slug)
	if err != nil {
		return codex.ThreadSettings{}, false, err
	}
	if summary.Codex.BindingSchema != 3 {
		return codex.ThreadSettings{}, true, errors.New("current provider manifest requirement changed")
	}
	if request.Cwd != "" && request.Cwd != metadata.Cwd || metadata.ID != request.ThreadID || !workspaceThread(metadata.Source) || metadata.Ephemeral == nil || *metadata.Ephemeral || summary.Archived || session.RequireRetainedReady(summary, c.socket) != nil {
		return codex.ThreadSettings{}, true, errors.New("current provider native identity changed")
	}
	if err := c.requireCurrentBindingPackage(); err != nil {
		return codex.ThreadSettings{}, true, err
	}
	if lineage.Current.Key.Slug != slug || lineage.Current.Key.RootThreadID != summary.Codex.ThreadID || lineage.Current.ThreadID != request.ThreadID || c.RetryPolicyResolver == nil {
		return codex.ThreadSettings{}, true, errors.New("current provider authority is unavailable")
	}
	if _, err := c.RetryPolicyResolver(ctx, request.ThreadID); err != nil {
		return codex.ThreadSettings{}, true, err
	}
	settings, err := c.ReadThreadSettings(ctx, request.ThreadID)
	if err != nil {
		return codex.ThreadSettings{}, true, err
	}
	settings.ModelProvider = lineage.Current.Provider
	return settings, true, nil
}

// CreatingMemberResume is supplied only by the locked member creation owner,
// after identity registration. Roster revision/project are that owner's proof;
// this adapter independently rechecks the provider binding and projections.
type CreatingMemberResume struct {
	Key              codexproviders.BindingKey
	ExpectedRevision uint64
	Provider         string
	RosterRevision   uint64
	ProjectID        string
}

type creatingMemberResumeKey struct{}

func (c *Client) ResumeRegisteredCreatingThread(ctx context.Context, proof CreatingMemberResume, environment map[string]string, settings codex.ThreadSettings) (string, error) {
	ctx = context.WithValue(ctx, creatingMemberResumeKey{}, proof)
	return c.Client.ResumeThreadWithSettings(ctx, proof.Key.OriginalThreadID, filepath.Join(proof.Key.Workspace, "work", proof.Key.Slug), environment, settings)
}

func (c *Client) creatingMemberProviderSettings(ctx context.Context, request codex.LifecycleRequest, proof CreatingMemberResume) (codex.ThreadSettings, error) {
	k := proof.Key
	if request.Method != "thread/resume" || request.ThreadID == "" || request.ThreadID != k.OriginalThreadID || request.Cwd != filepath.Join(c.workspace, "work", k.Slug) ||
		k.Workspace != c.workspace || !session.ValidSlug(k.Slug) || k.Member == "lead" || !codexproviders.ValidID(k.Member) || proof.RosterRevision == 0 || proof.ProjectID == "" || proof.ExpectedRevision == 0 {
		return codex.ThreadSettings{}, errors.New("creating member resume purpose changed")
	}
	summary, err := session.Find(c.workspace, k.Slug)
	if err != nil || summary.Archived || summary.Schema != 6 || summary.Codex.BindingSchema != 3 || summary.Codex.ThreadID != k.RootThreadID || summary.Codex.SocketPath != c.socket {
		return codex.ThreadSettings{}, errors.New("creating member origin changed")
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		return codex.ThreadSettings{}, err
	}
	state, err := codexproviders.ReadBindingState(c.RecoveryRoot, os.Getenv("DEV_WORKSPACE_NAME"), c.workspace, c.socket, catalog)
	if err != nil {
		return codex.ThreadSettings{}, err
	}
	authority, err := session.LoadRuntimeAuthority(filepath.Join(filepath.Dir(c.socket), "authority"), k.Slug, c.workspace)
	if err != nil {
		return codex.ThreadSettings{}, err
	}
	b, ok := state.Bindings[k.Slug]
	member, found := b.Conversations[k.OriginalThreadID]
	if !ok || !found || state.Schema != 3 || state.BlocksSession(k.Slug, k.RootThreadID) || b.RootThreadID != k.RootThreadID || b.Incarnation != k.Incarnation || b.Revision != proof.ExpectedRevision ||
		state.Provider(k.Slug, k.RootThreadID) != proof.Provider || member.Member != k.Member || member.ThreadID != k.OriginalThreadID || member.Provider != proof.Provider || member.OperationID != "" || member.EvidenceDigest != "" ||
		authority.Schema != 3 || authority.State != "ready" || authority.CodexThreadID != k.RootThreadID || authority.CodexSocketPath != c.socket || authority.TmuxIdentity != k.Incarnation ||
		authority.CodexBindingRevision != b.Revision || authority.CodexActiveThreadID != b.Conversations[k.RootThreadID].ThreadID {
		return codex.ThreadSettings{}, errors.New("creating member registration changed")
	}
	retained, err := c.ReadThreadSettings(ctx, k.OriginalThreadID)
	if err != nil {
		return codex.ThreadSettings{}, err
	}
	return codex.ThreadSettings{ModelProvider: proof.Provider, Model: retained.Model, ReasoningEffort: retained.ReasoningEffort}, nil
}

// RetainedRootRecovery is constructed by the existing locked start/revive
// owner. It grants one exact retained operation, without ready-caller authority.
type RetainedRootRecovery struct {
	Key              codexproviders.BindingKey
	NativeThreadID   string
	ExpectedRevision uint64
	Provider         string
}
type retainedRootRecoveryKey struct{}

type currentLeadInstructions struct {
	Current codexproviders.ResolvedBinding
	Package string
	Policy  codex.ThreadPolicy
}
type currentLeadInstructionsKey struct{}

func (c *Client) validateCurrentLeadInstructions(ctx context.Context, proof currentLeadInstructions) error {
	current, key := proof.Current, proof.Current.Key
	if key.Workspace != c.workspace || !session.ValidSlug(key.Slug) || key.Member != "lead" || key.OriginalThreadID != key.RootThreadID || key.RootThreadID == "" || current.Socket != c.socket || current.ThreadID == "" || current.Revision == 0 || !c.RetryAvailable() {
		return errors.New("current instruction purpose is incomplete")
	}
	if err := c.maintenanceAdmission(); err != nil {
		return err
	}
	selected, err := filepath.EvalSymlinks(c.maintenanceProfile())
	if err != nil || !filepath.IsAbs(proof.Package) || filepath.Clean(proof.Package) != proof.Package || selected != proof.Package {
		return errors.New("current instruction package changed")
	}
	data, err := os.ReadFile(filepath.Join(selected, "share", "workspace-portal", "runtime-contract.json"))
	var contract map[string]json.RawMessage
	if err != nil || session.DecodePiCreationJSON(data, &contract, 64<<10) != nil || string(contract["codexProviderBindingStateSchema"]) != "3" || string(contract["codexProviderForkTransitionPolicy"]) != "1" || string(contract["codexRuntimeAuthoritySchema"]) != "3" {
		return errors.New("current instruction package cannot admit bindings")
	}
	summary, err := session.Find(c.workspace, key.Slug)
	if err != nil || summary.Archived || summary.Schema != 6 || summary.Codex.BindingSchema != 3 || summary.Codex.ThreadID != key.RootThreadID || session.RequireRetainedReady(summary, c.socket) != nil {
		return errors.New("current instruction origin changed")
	}
	if owner, err := session.PendingLifecycle(c.workspace, key.Slug); err != nil || owner != "" {
		return errors.New("current instruction has an unfinished lifecycle")
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		return err
	}
	state, err := codexproviders.ReadBindingState(c.RecoveryRoot, filepath.Base(filepath.Dir(c.socket)), c.workspace, c.socket, catalog)
	if err != nil || state.BlocksSession(key.Slug, key.RootThreadID) {
		return errors.New("current instruction binding is unavailable or fenced")
	}
	authority, err := session.LoadRuntimeAuthority(filepath.Join(filepath.Dir(c.socket), "authority"), key.Slug, c.workspace)
	if err != nil || authority.Schema != 3 {
		return errors.New("current instruction requires ready binding authority")
	}
	again, err := authority.ResolveCodexBinding(summary, state, "lead", key.RootThreadID, true)
	if err != nil || again != current {
		return errors.New("current instruction route changed")
	}
	policy, err := c.RetryPolicyResolver(ctx, current.ThreadID)
	if err != nil || !reflect.DeepEqual(policy, proof.Policy) {
		return errors.New("current instruction policy changed")
	}
	metadata, err := c.ReadThreadMetadata(ctx, current.ThreadID, false)
	if err != nil || metadata.ID != current.ThreadID || metadata.Cwd != filepath.Join(c.workspace, "work", key.Slug) || !workspaceThread(metadata.Source) || metadata.Ephemeral == nil || *metadata.Ephemeral {
		return errors.New("current instruction native identity changed")
	}
	return nil
}

// The web owner retains transition/runtime/team exclusion for this one managed
// instruction operation. Validate before entry: a warm SDK settings write occurs
// before the resume provider callback. The purpose cannot escape this call.
func (c *Client) ReconcileCurrentLeadInstructions(ctx context.Context, current codexproviders.ResolvedBinding, selectedPackage string, policy codex.ThreadPolicy) error {
	proof := currentLeadInstructions{Current: current, Package: selectedPackage, Policy: policy}
	if err := c.validateCurrentLeadInstructions(ctx, proof); err != nil {
		return err
	}
	cwd := filepath.Join(c.workspace, "work", current.Key.Slug)
	if state, err := c.ProveMaterializedActiveThread(ctx, current.ThreadID, cwd); err != nil || state != ArchiveActive {
		return errors.New("current instruction root is not materialized active history")
	}
	if err := c.RequireThreadIdle(ctx, current.ThreadID, cwd); err != nil {
		return err
	}
	if err := c.RequireSubmissionAttemptsResolved(ctx, current.ThreadID); err != nil {
		return err
	}
	if err := c.validateCurrentLeadInstructions(ctx, proof); err != nil {
		return err
	}
	return c.ReconcileThreadInstructionsWithPolicy(context.WithValue(ctx, currentLeadInstructionsKey{}, proof), current.ThreadID, policy)
}

func (c *Client) retainedRootRecoveryProof(proof RetainedRootRecovery) error {
	k := proof.Key
	if k.Workspace != c.workspace || !session.ValidSlug(k.Slug) || k.Member != "lead" || k.OriginalThreadID != k.RootThreadID || k.RootThreadID == "" || proof.NativeThreadID == "" || proof.ExpectedRevision == 0 {
		return errors.New("retained root recovery purpose changed")
	}
	summary, err := session.Find(c.workspace, k.Slug)
	if err != nil || summary.Archived || summary.Schema != 6 || summary.Codex.BindingSchema != 3 || summary.Codex.ThreadID != k.RootThreadID || summary.Codex.SocketPath != c.socket || summary.HasCreation() && (summary.Creation.State != "ready" || summary.Creation.GoalSHA256 != "" && !summary.Creation.InitialGoalSent) {
		return errors.New("retained root recovery origin changed")
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		return err
	}
	name := filepath.Base(filepath.Dir(c.socket))
	if filepath.Base(c.socket) != "app-server.sock" || !codexproviders.ValidID(name) {
		return errors.New("retained root runtime locator changed")
	}
	state, err := codexproviders.ReadBindingState(c.RecoveryRoot, name, c.workspace, c.socket, catalog)
	if err != nil {
		return err
	}
	b, ok := state.Bindings[k.Slug]
	if !ok || b.RootThreadID != k.RootThreadID || b.Incarnation != k.Incarnation || b.Revision != proof.ExpectedRevision || state.Provider(k.Slug, k.RootThreadID) != proof.Provider || state.BlocksSession(k.Slug, k.RootThreadID) {
		return errors.New("retained root binding changed or is fenced")
	}
	targets, err := state.ReceiptTargets(k, true, func(op string) ([]byte, error) { return codexproviders.ReadBindingEvidence(c.RecoveryRoot, name, op) })
	if err != nil {
		return err
	}
	if len(targets) == 0 || targets[0].ThreadID != proof.NativeThreadID || targets[0].Provider != proof.Provider {
		return errors.New("retained root native target changed")
	}
	authority, err := session.LoadRuntimeAuthority(filepath.Join(filepath.Dir(c.socket), "authority"), k.Slug, c.workspace)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && (authority.Schema != 3 || authority.State != "ready" || authority.CodexThreadID != k.RootThreadID || authority.CodexActiveThreadID != proof.NativeThreadID || authority.CodexBindingRevision != b.Revision || authority.TmuxIdentity != k.Incarnation || authority.CodexSocketPath != c.socket) {
		return errors.New("retained recovery authority changed")
	}
	return nil
}

func (c *Client) ResumeRetainedRoot(ctx context.Context, proof RetainedRootRecovery, environment map[string]string, settings codex.ThreadSettings) (string, error) {
	if err := c.maintenanceAdmission(); err != nil {
		return "", err
	}
	if err := c.retainedRootRecoveryProof(proof); err != nil {
		return "", err
	}
	state, err := c.ProveMaterializedActiveThread(ctx, proof.NativeThreadID, filepath.Join(c.workspace, "work", proof.Key.Slug))
	if err != nil {
		return "", err
	}
	if state != ArchiveActive {
		return "", errors.New("retained root is not materialized active history")
	}
	if err := c.retainedRootRecoveryProof(proof); err != nil {
		return "", err
	}
	scoped := context.WithValue(ctx, retainedRootRecoveryKey{}, proof)
	id, err := c.Client.ResumeThreadWithSettings(scoped, proof.NativeThreadID, filepath.Join(c.workspace, "work", proof.Key.Slug), environment, settings)
	if err == nil && id != proof.NativeThreadID {
		return "", errors.New("retained resume returned another native target")
	}
	return id, err
}

func (c *Client) RecoverArchivedRetainedRoot(ctx context.Context, proof RetainedRootRecovery, environment map[string]string, settings codex.ThreadSettings) (string, error) {
	if err := c.maintenanceAdmission(); err != nil {
		return "", err
	}
	if err := c.retainedRootRecoveryProof(proof); err != nil {
		return "", err
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		return "", err
	}
	name := filepath.Base(filepath.Dir(c.socket))
	state, err := codexproviders.ReadBindingState(c.RecoveryRoot, name, c.workspace, c.socket, catalog)
	if err != nil {
		return "", err
	}
	targets, err := state.ReceiptTargets(proof.Key, true, func(op string) ([]byte, error) { return codexproviders.ReadBindingEvidence(c.RecoveryRoot, name, op) })
	if err != nil {
		return "", err
	}
	archived, err := c.retainedRootRecoveryDiscovery(ctx, proof, targets)
	if err != nil {
		return "", err
	}
	if err := c.retainedRootRecoveryProof(proof); err != nil {
		return "", err
	}
	again, err := c.retainedRootRecoveryDiscovery(ctx, proof, targets)
	if err != nil || again != archived {
		return "", errors.New("retained root discovery changed before recovery")
	}
	if err := c.retainedRootRecoveryProof(proof); err != nil {
		return "", err
	}
	if err := c.RequireThreadTurnsIdle(ctx, proof.NativeThreadID); err != nil {
		return "", err
	}
	scoped := context.WithValue(ctx, retainedRootRecoveryKey{}, proof)
	if archived {
		thread, err := c.UnarchiveThread(scoped, proof.NativeThreadID)
		if err != nil {
			return "", err
		}
		if thread.ID != proof.NativeThreadID || thread.Cwd != filepath.Join(c.workspace, "work", proof.Key.Slug) || !workspaceThread(thread.Source) || thread.Ephemeral == nil || *thread.Ephemeral {
			return "", errors.New("retained unarchive returned another native identity")
		}
	}
	id, err := c.Client.ResumeThreadWithSettings(scoped, proof.NativeThreadID, filepath.Join(c.workspace, "work", proof.Key.Slug), environment, settings)
	if err == nil && id != proof.NativeThreadID {
		return "", errors.New("retained recovery returned another native target")
	}
	return id, err
}

// Complete root discovery confirms a frozen sealed set. Structured team/native
// sources keep the legacy root filter; no discovered ID becomes a new owner.
func (c *Client) retainedRootRecoveryDiscovery(ctx context.Context, proof RetainedRootRecovery, targets []codexproviders.ReceiptTarget) (bool, error) {
	cwd := filepath.Join(c.workspace, "work", proof.Key.Slug)
	allowed := map[string]bool{}
	for _, target := range targets {
		allowed[target.ThreadID] = true
	}
	seen := map[string]codex.ThreadMetadata{}
	archivedIDs := map[string]bool{}
	validate := func(m codex.ThreadMetadata) error {
		if !allowed[m.ID] || m.Cwd != cwd || !workspaceThread(m.Source) || m.Ephemeral == nil || *m.Ephemeral {
			return errors.New("unknown or changed retained root discovery identity")
		}
		return nil
	}
	for _, archived := range []bool{false, true} {
		cursors := map[string]bool{}
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > len(targets) {
				return false, errors.New("retained root discovery exceeded sealed set")
			}
			rows, next, err := c.retirementThreads(ctx, cwd, archived, false, len(targets)+1, cursor)
			if err != nil {
				return false, err
			}
			for _, m := range rows {
				if err := validate(m); err != nil {
					return false, err
				}
				if _, ok := seen[m.ID]; ok {
					return false, errors.New("duplicate or mixed active/archived retained root")
				}
				seen[m.ID] = m
				archivedIDs[m.ID] = archived
			}
			if next == nil {
				break
			}
			if *next == "" || cursors[*next] {
				return false, errors.New("incomplete or repeated retained root cursor")
			}
			cursors[*next] = true
			cursor = *next
		}
	}
	loaded, err := c.LoadedThreadIDs(ctx)
	if err != nil {
		return false, err
	}
	loadedIDs := map[string]bool{}
	for _, id := range loaded {
		if !observationID(id, true) || loadedIDs[id] {
			return false, errors.New("duplicate or invalid loaded root identity")
		}
		loadedIDs[id] = true
		m, err := c.ReadThreadMetadata(ctx, id, false)
		if err != nil {
			return false, err
		}
		if m.ID != id || !canonicalArchivePath(m.Cwd) || m.Source == nil {
			return false, errors.New("loaded recovery identity cannot be verified")
		}
		if !allowed[id] && (m.Cwd != cwd || !workspaceThread(m.Source)) {
			continue
		}
		if err := validate(m); err != nil {
			return false, err
		}
		if old, ok := seen[id]; ok && (archivedIDs[id] || old.Cwd != m.Cwd || !reflect.DeepEqual(old.Source, m.Source) || !reflect.DeepEqual(old.ProjectID, m.ProjectID) || !reflect.DeepEqual(old.Path, m.Path)) {
			return false, errors.New("saved and loaded root identities disagree")
		}
		seen[id] = m
	}
	if len(seen) != len(allowed) {
		return false, errors.New("sealed root history is missing from complete discovery")
	}
	for _, target := range targets {
		id := target.ThreadID
		state, err := c.ProveArchivedRootThread(ctx, id, cwd)
		if err != nil {
			return false, err
		}
		if archivedIDs[id] && state != ArchiveArchived || !archivedIDs[id] && state != ArchiveActive {
			return false, errors.New("retained root archive state disagrees with discovery")
		}
		if id != proof.NativeThreadID {
			if err := c.RequireSubmissionAttemptsResolved(ctx, id); err != nil {
				return false, err
			}
			if err := c.RequireArchiveThreadIdle(ctx, id, cwd, state); err != nil {
				return false, fmt.Errorf("sealed ancestor is not idle: %w", err)
			}
		}
	}
	return archivedIDs[proof.NativeThreadID], nil
}

// AcceptedBoundFork separates immutable source identity from the destination
// creation selection. Its journal/roster eligibility is proved by the locked
// owning main/team caller, never inferred here from a native ID.
type AcceptedBoundFork struct {
	SourceKey               codexproviders.BindingKey
	SourceThreadID          string
	SourceRevision          uint64
	SourceProvider          string
	DestinationSlug         string
	DestinationIncarnation  string
	DestinationRootThreadID string
	DestinationProvider     string
	SourceForkDigest        string
	SourcePackage           string
}
type acceptedBoundForkKey struct{}

func (c *Client) acceptedBoundForkProof(proof AcceptedBoundFork) error {
	k := proof.SourceKey
	if k.Workspace != c.workspace || k.Slug == proof.DestinationSlug || !session.ValidSlug(k.Slug) || !session.ValidSlug(proof.DestinationSlug) || proof.SourceThreadID == "" || proof.SourceRevision == 0 || len(proof.SourceForkDigest) != 64 {
		return errors.New("accepted fork purpose changed")
	}
	selected, err := filepath.EvalSymlinks(c.maintenanceProfile())
	if err != nil || selected != proof.SourcePackage || !filepath.IsAbs(selected) || filepath.Clean(selected) != selected {
		return errors.New("accepted fork package generation changed")
	}
	data, err := os.ReadFile(filepath.Join(selected, "share", "workspace-portal", "runtime-contract.json"))
	var contract map[string]json.RawMessage
	if err != nil || session.DecodePiCreationJSON(data, &contract, 64<<10) != nil || string(contract["codexProviderBindingStateSchema"]) != "3" || string(contract["codexProviderForkTransitionPolicy"]) != "1" || string(contract["codexRuntimeAuthoritySchema"]) != "3" {
		return errors.New("accepted fork consumers are unavailable")
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		return err
	}
	name := filepath.Base(filepath.Dir(c.socket))
	if filepath.Base(c.socket) != "app-server.sock" || !codexproviders.ValidID(name) {
		return errors.New("accepted fork socket locator changed")
	}
	state, err := codexproviders.ReadBindingState(c.RecoveryRoot, name, c.workspace, c.socket, catalog)
	if err != nil {
		return err
	}
	source, err := session.Find(c.workspace, k.Slug)
	if err != nil || source.Schema != 6 || source.Archived || source.Codex.BindingSchema != 3 || source.Codex.ThreadID != k.RootThreadID || source.Codex.SocketPath != c.socket || session.RequireRetainedReady(source, c.socket) != nil {
		return errors.New("accepted fork source origin changed")
	}
	binding, ok := state.Bindings[k.Slug]
	if !ok || binding.RootThreadID != k.RootThreadID || binding.Incarnation != k.Incarnation || binding.Revision != proof.SourceRevision || state.Provider(k.Slug, k.RootThreadID) != proof.SourceProvider || state.BlocksSession(k.Slug, k.RootThreadID) || state.BlocksSession(proof.DestinationSlug, proof.DestinationRootThreadID) {
		return errors.New("accepted fork binding changed or is fenced")
	}
	authority, err := session.LoadRuntimeAuthority(filepath.Join(filepath.Dir(c.socket), "authority"), k.Slug, c.workspace)
	if err != nil {
		return err
	}
	root, err := authority.ResolveCodexBinding(source, state, "lead", k.RootThreadID, true)
	if err != nil || root.Revision != proof.SourceRevision || root.Key.Incarnation != k.Incarnation {
		return errors.New("accepted fork source authority changed")
	}
	targets, err := state.ReceiptTargets(k, true, func(op string) ([]byte, error) { return codexproviders.ReadBindingEvidence(c.RecoveryRoot, name, op) })
	if err != nil {
		return err
	}
	if len(targets) == 0 || targets[0].ThreadID != proof.SourceThreadID || targets[0].Provider != proof.SourceProvider {
		return errors.New("accepted fork source native target changed")
	}
	path := filepath.Join(c.workspace, "worktrees", ".locks", proof.DestinationSlug+".fork.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 64<<10 {
		return errors.New("accepted destination journal is unavailable")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		return err
	}
	var journal map[string]json.RawMessage
	if json.Unmarshal(data, &journal) != nil {
		return errors.New("accepted destination journal is invalid")
	}
	sum := sha256.Sum256(journal["source_fork"])
	var slug, sourceSlug, sourceRoot, incarnation, phase string
	if hex.EncodeToString(sum[:]) != proof.SourceForkDigest || json.Unmarshal(journal["slug"], &slug) != nil || json.Unmarshal(journal["source_slug"], &sourceSlug) != nil || json.Unmarshal(journal["source_thread_id"], &sourceRoot) != nil || json.Unmarshal(journal["tmux_identity"], &incarnation) != nil || json.Unmarshal(journal["state"], &phase) != nil || slug != proof.DestinationSlug || sourceSlug != k.Slug || sourceRoot != k.RootThreadID || incarnation != proof.DestinationIncarnation || phase != "creating" {
		return errors.New("accepted destination selection changed")
	}
	var progress struct {
		State    string `json:"state"`
		ThreadID string `json:"threadId,omitempty"`
	}
	if session.DecodePiCreationJSON(journal["root_fork"], &progress, 1024) != nil ||
		(k.Member == "lead" && (progress.State != "intent" || progress.ThreadID != "" || proof.DestinationRootThreadID != "")) ||
		(k.Member != "lead" && (progress.State != "known" || progress.ThreadID == "" || progress.ThreadID != proof.DestinationRootThreadID)) {
		return errors.New("accepted destination root progress changed")
	}
	destination, err := session.Find(c.workspace, proof.DestinationSlug)
	if err != nil || destination.Archived || destination.Codex.ThreadID != proof.DestinationRootThreadID || proof.DestinationRootThreadID != "" && destination.Codex.SocketPath != c.socket {
		return errors.New("accepted destination origin changed")
	}
	selectedProvider := state.Provider(proof.DestinationSlug, proof.DestinationRootThreadID)
	selection, explicit := state.Sessions[proof.DestinationSlug]
	var requestedProvider string
	if raw, present := journal["provider"]; present {
		if json.Unmarshal(raw, &requestedProvider) != nil || requestedProvider == "" {
			return errors.New("accepted destination provider is invalid")
		}
	}
	if requestedProvider == "" {
		if explicit || proof.DestinationProvider != state.Active {
			return errors.New("inherited destination selection changed")
		}
	} else if !explicit || selection.Provider != requestedProvider || selection.CreationID != proof.DestinationIncarnation || selection.RootThreadID != "" && selection.RootThreadID != proof.DestinationRootThreadID {
		return errors.New("explicit destination selection changed")
	}
	if requestedProvider != "" {
		selectedProvider = selection.Provider
	}
	if selectedProvider != proof.DestinationProvider {
		return errors.New("accepted destination provider changed")
	}
	if b, ok := state.Bindings[proof.DestinationSlug]; ok && (b.RootThreadID != proof.DestinationRootThreadID || b.Incarnation != proof.DestinationIncarnation || b.Conversations[b.RootThreadID].ThreadID != proof.DestinationRootThreadID) {
		return errors.New("destination already has a different binding")
	}
	return nil
}

func (c *Client) ForkAcceptedBoundSource(ctx context.Context, proof AcceptedBoundFork, environment map[string]string, settings codex.ThreadSettings) (string, error) {
	if err := c.maintenanceAdmission(); err != nil {
		return "", err
	}
	if err := c.acceptedBoundForkProof(proof); err != nil {
		return "", err
	}
	scoped := context.WithValue(ctx, acceptedBoundForkKey{}, proof)
	return c.Client.ForkThread(scoped, proof.SourceThreadID, filepath.Join(c.workspace, "work", proof.DestinationSlug), environment, settings)
}

func (c *Client) providerRequestSettings(ctx context.Context, request codex.LifecycleRequest) (codex.ThreadSettings, error) {
	if err := c.maintenanceAdmission(); err != nil {
		return codex.ThreadSettings{}, err
	}
	if proof, ok := ctx.Value(currentLeadInstructionsKey{}).(currentLeadInstructions); ok {
		if request.Method != "thread/resume" || request.ThreadID != proof.Current.ThreadID || request.Cwd != "" && request.Cwd != filepath.Join(c.workspace, "work", proof.Current.Key.Slug) {
			return codex.ThreadSettings{}, errors.New("current instruction lifecycle purpose changed")
		}
		if err := c.validateCurrentLeadInstructions(ctx, proof); err != nil {
			return codex.ThreadSettings{}, err
		}
		settings, err := c.ReadThreadSettings(ctx, request.ThreadID)
		if err != nil {
			return codex.ThreadSettings{}, err
		}
		return codex.ThreadSettings{ModelProvider: proof.Current.Provider, Model: settings.Model, ReasoningEffort: settings.ReasoningEffort}, nil
	}
	if proof, ok := ctx.Value(acceptedBoundForkKey{}).(AcceptedBoundFork); ok {
		if request.Method != "thread/fork" || request.ThreadID != proof.SourceThreadID || request.Cwd != filepath.Join(c.workspace, "work", proof.DestinationSlug) {
			return codex.ThreadSettings{}, errors.New("accepted fork lifecycle purpose changed")
		}
		if err := c.acceptedBoundForkProof(proof); err != nil {
			return codex.ThreadSettings{}, err
		}
		return codex.ThreadSettings{ModelProvider: proof.DestinationProvider}, nil
	}
	if proof, ok := ctx.Value(retainedRootRecoveryKey{}).(RetainedRootRecovery); ok {
		if request.Method != "thread/resume" || request.ThreadID != proof.NativeThreadID || request.ThreadID == "" || request.Cwd != filepath.Join(c.workspace, "work", proof.Key.Slug) {
			return codex.ThreadSettings{}, errors.New("retained recovery lifecycle purpose changed")
		}
		if err := c.retainedRootRecoveryProof(proof); err != nil {
			return codex.ThreadSettings{}, err
		}
		settings, err := c.ReadThreadSettings(ctx, proof.NativeThreadID)
		if err != nil {
			return codex.ThreadSettings{}, err
		}
		return codex.ThreadSettings{ModelProvider: proof.Provider, Model: settings.Model, ReasoningEffort: settings.ReasoningEffort}, nil
	}
	if proof, ok := ctx.Value(creatingMemberResumeKey{}).(CreatingMemberResume); ok {
		return c.creatingMemberProviderSettings(ctx, request, proof)
	}
	if settings, marked, err := c.markedResumeSettings(ctx, request); marked || err != nil {
		return settings, err
	}
	state, err := c.providerState()
	if err != nil {
		return codex.ThreadSettings{}, err
	}
	if state == nil {
		return codex.ThreadSettings{}, nil
	}
	if state.Schema == 3 && request.Method == "thread/fork" && request.ThreadID != "" {
		metadata, err := c.ReadThreadMetadata(ctx, request.ThreadID, false)
		if err != nil {
			return codex.ThreadSettings{}, err
		}
		slug := filepath.Base(metadata.Cwd)
		source, err := session.Find(c.workspace, slug)
		if err != nil || metadata.Cwd != filepath.Join(c.workspace, "work", slug) || source.Codex.BindingSchema == 3 {
			return codex.ThreadSettings{}, errors.New("marked source fork requires its exact accepted operation")
		}
		if _, bound := state.Bindings[slug]; bound {
			return codex.ThreadSettings{}, errors.New("fork source binding has no marked owner")
		}
	}
	thread, cwd := request.ThreadID, request.Cwd
	if cwd == "" && thread != "" {
		var response struct {
			Thread struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			} `json:"thread"`
		}
		if err := c.Request(ctx, "thread/read", map[string]any{"threadId": thread, "excludeTurns": true}, &response); err != nil {
			return codex.ThreadSettings{}, err
		}
		if response.Thread.ID != thread {
			return codex.ThreadSettings{}, errors.New("provider thread identity changed")
		}
		cwd = response.Thread.Cwd
	}
	provider := state.Active
	if cwd != "" {
		slug := filepath.Base(cwd)
		if !session.ValidSlug(slug) || cwd != filepath.Join(c.workspace, "work", slug) {
			return codex.ThreadSettings{}, errors.New("provider destination is outside its session")
		}
		summary, err := session.Find(c.workspace, slug)
		if err != nil || summary.Archived || (summary.Codex.ThreadID != "" && summary.Codex.SocketPath != c.socket) {
			return codex.ThreadSettings{}, errors.New("provider session identity changed")
		}
		fenceRoot := summary.Codex.ThreadID
		provider = state.Provider(slug, summary.Codex.ThreadID)
		if selection, ok := state.Sessions[slug]; ok && selection.CreationID != "" && selection.RootThreadID == "" {
			var path string
			for _, suffix := range []string{".creation.json", ".fork.json"} {
				candidate := filepath.Join(c.workspace, "worktrees", ".locks", slug+suffix)
				if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil || path != "" {
					return codex.ThreadSettings{}, errors.New("provider creation identity is unavailable or ambiguous")
				}
				path = candidate
			}
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 64*1024 {
				return codex.ThreadSettings{}, errors.New("provider creation identity is unavailable")
			}
			data, err := os.ReadFile(path)
			var journal struct {
				Slug     string `json:"slug"`
				Schema   int    `json:"schema"`
				Provider string `json:"provider"`
				State    string `json:"state"`
				Identity string `json:"tmux_identity"`
			}
			if err != nil || json.Unmarshal(data, &journal) != nil || journal.Slug != slug || journal.State != "creating" || journal.Identity != selection.CreationID {
				return codex.ThreadSettings{}, errors.New("provider creation incarnation changed")
			}
			if strings.HasSuffix(path, ".fork.json") && (journal.Schema != 2 || journal.Provider != selection.Provider) {
				return codex.ThreadSettings{}, errors.New("provider fork selection changed")
			}
			provider = selection.Provider
			// The root may have just reached the manifest while bind-root is
			// still pending. The verified creation incarnation owns this window.
			fenceRoot = ""
		}
		if state.BlocksSession(slug, fenceRoot) {
			return codex.ThreadSettings{}, errors.New("this session is changing provider; its work will resume automatically")
		}
		if summary.Codex.BindingSchema == 3 {
			authority, err := session.LoadRuntimeAuthority(filepath.Join(filepath.Dir(c.socket), "authority"), slug, c.workspace)
			if err != nil {
				return codex.ThreadSettings{}, err
			}
			if _, err := authority.ResolveCodexBinding(summary, *state, "lead", summary.Codex.ThreadID, true); err != nil {
				return codex.ThreadSettings{}, err
			}
			if thread != "" {
				if c.RetryPolicyResolver == nil {
					return codex.ThreadSettings{}, errors.New("current conversation authority is unavailable")
				}
				if _, err := c.RetryPolicyResolver(ctx, thread); err != nil {
					return codex.ThreadSettings{}, err
				}
				matched := false
				for original, conversation := range state.Bindings[slug].Conversations {
					if conversation.ThreadID != thread {
						continue
					}
					current, err := authority.ResolveCodexBinding(summary, *state, conversation.Member, original, true)
					if err != nil || current.ThreadID != thread {
						return codex.ThreadSettings{}, errors.New("current conversation binding changed")
					}
					matched = true
				}
				if !matched {
					return codex.ThreadSettings{}, errors.New("historical target cannot be loaded as current")
				}
			} else if request.Method != "thread/start" {
				return codex.ThreadSettings{}, errors.New("current lifecycle target is missing")
			}
		}
	} else if state.Operation.Reserved() {
		return codex.ThreadSettings{}, errors.New("provider scope is unavailable during transition")
	}
	settings := codex.ThreadSettings{ModelProvider: provider}
	if thread != "" {
		retained, err := c.ReadThreadSettings(ctx, thread)
		if err != nil {
			return codex.ThreadSettings{}, err
		}
		settings.Model = retained.Model
		settings.ReasoningEffort = retained.ReasoningEffort
	}
	return settings, nil
}

func ResolveNewThreadSettings(
	models []codex.Model, requested codex.ThreadSettings,
) (codex.ThreadSettings, error) {
	if requested.Model == "" {
		// The App Server resolves any omitted model and effort from its live
		// configuration. An effort-only request is also server-owned because this
		// layer does not know which configured default model will receive it.
		return requested, nil
	}

	var selected *codex.Model
	for index := range models {
		candidate := &models[index]
		if candidate.Model != requested.Model {
			continue
		}
		if selected != nil {
			return codex.ThreadSettings{}, fmt.Errorf(
				"Codex model catalog has more than one %q model", requested.Model,
			)
		}
		selected = candidate
	}
	if selected == nil {
		return codex.ThreadSettings{}, fmt.Errorf("Codex model %q is not available", requested.Model)
	}
	if requested.ReasoningEffort == "" {
		return requested, nil
	}
	for _, effort := range selected.SupportedReasoningEfforts {
		if effort.ReasoningEffort == requested.ReasoningEffort {
			return requested, nil
		}
	}
	return codex.ThreadSettings{}, fmt.Errorf(
		"reasoning effort %q is not available for %s", requested.ReasoningEffort, selected.DisplayName,
	)
}

func (c *Client) RecoverCreatingThread(
	ctx context.Context, threadID, cwd string, environment map[string]string,
) (string, error) {
	return c.RecoverCreatingThreadWithSettings(ctx, threadID, cwd, environment, codex.ThreadSettings{})
}

func (c *Client) RecoverCreatingThreadWithSettings(
	ctx context.Context,
	threadID, cwd string,
	environment map[string]string,
	settings codex.ThreadSettings,
) (string, error) {
	return c.RecoverCreatingThreadWithPolicyAndSettingsResolver(
		ctx, threadID, cwd, environment, settings.Policy,
		func() (codex.ThreadSettings, error) { return settings, nil },
	)
}

// RecoverCreatingThreadWithSettingsResolver keeps creation retries bound to
// one development-session directory. Existing threads retain their original
// settings; defaults are resolved only when a replacement must be created.
func (c *Client) RecoverCreatingThreadWithSettingsResolver(
	ctx context.Context,
	threadID, cwd string,
	environment map[string]string,
	resolveSettings func() (codex.ThreadSettings, error),
) (string, error) {
	return c.RecoverCreatingThreadWithPolicyAndSettingsResolver(
		ctx, threadID, cwd, environment, codex.ThreadPolicy{}, resolveSettings,
	)
}

// RecoverCreatingThreadWithPolicyAndSettingsResolver retains an unmaterialized
// candidate's start-time policy and refreshes a materialized candidate's policy
// without replacing its saved model or effort. Only a new root calls the resolver.
func (c *Client) RecoverCreatingThreadWithPolicyAndSettingsResolver(
	ctx context.Context,
	threadID, cwd string,
	environment map[string]string,
	policy codex.ThreadPolicy,
	resolveSettings func() (codex.ThreadSettings, error),
) (string, error) {
	return c.RecoverCreatingThreadWithOptions(ctx, threadID, cwd, environment, policy, resolveSettings, RecoveryOptions{})
}

// RecoveryOptions contains only application-validated roster identities. The
// composition layer holds the team operation lock until discovery has finished.
type RecoveryOptions struct {
	ExcludedThreads map[string]string // exact thread ID -> retained project ID (empty for legacy members)
	RetainedRoster  bool
	BeforeCreate    func() // releases the roster operation lock before any new thread
	Progress        creationprogress.Observer
}

func (c *Client) RecoverCreatingThreadWithOptions(
	ctx context.Context, threadID, cwd string, environment map[string]string,
	policy codex.ThreadPolicy, resolveSettings func() (codex.ThreadSettings, error), options RecoveryOptions,
) (string, error) {
	candidates, err := c.creationCandidates(ctx, threadID, cwd, options)
	if err != nil {
		return "", err
	}
	for candidateID, candidate := range candidates {
		if candidateID != threadID && options.RetainedRoster {
			return "", errors.New("retained team roster prevents root thread replacement")
		}
		if candidate.ForkedFromID != "" {
			return "", errors.New("refusing a fork as a creation replacement")
		}
		materialized, err := c.HistoryMaterialized(ctx, candidateID, cwd)
		if err != nil {
			return "", err
		}
		if candidateID != threadID && materialized {
			return "", errors.New("refusing a different materialized Codex thread as a creation replacement")
		}
		if candidateID != threadID {
			if err := c.requireMissingCreationThread(ctx, threadID, cwd); err != nil {
				return "", err
			}
		}
		if !materialized {
			// Selected Codex requires a rollout even to resume a loaded thread.
			// This proven fresh root still has its original policy and environment.
			return candidateID, nil
		}
		return c.ResumeThreadWithSettings(ctx, candidateID, cwd, environment, codex.ThreadSettings{Policy: policy})
	}
	if options.RetainedRoster {
		return "", errors.New("retained team roster prevents root thread replacement")
	}
	if err := c.requireMissingCreationThread(ctx, threadID, cwd); err != nil {
		return "", err
	}
	settings, err := resolveSettings()
	if err != nil {
		return "", err
	}
	if options.BeforeCreate != nil {
		options.BeforeCreate()
	}
	return c.StartThreadWithSettings(ctx, cwd, environment, settings)
}

func (c *Client) requireMissingCreationThread(ctx context.Context, threadID, cwd string) error {
	if threadID == "" {
		return nil
	}
	// Filtered discovery cannot establish that a recorded ID with changed
	// cwd/source is gone. Only an exact not-found result permits replacement.
	metadata, err := c.ReadThreadMetadata(ctx, threadID, false)
	if err == nil {
		if metadata.Cwd != cwd || !workspaceThread(metadata.Source) {
			return errors.New("recorded creation thread has the wrong identity")
		}
		return errors.New("recorded creation thread is absent from complete discovery")
	}
	if !codex.IsThreadNotFound(err, threadID) {
		return err
	}
	return nil
}

func (c *Client) RecoverForkThread(
	ctx context.Context, sourceThreadID, cwd string, environment map[string]string, settings codex.ThreadSettings,
) (string, error) {
	return c.RecoverForkThreadWithOptions(ctx, sourceThreadID, cwd, environment, settings, RecoveryOptions{})
}

func (c *Client) RecoverForkThreadWithOptions(
	ctx context.Context, sourceThreadID, cwd string, environment map[string]string, settings codex.ThreadSettings, options RecoveryOptions,
) (string, error) {
	candidates, err := c.creationCandidates(ctx, "", cwd, options)
	if err != nil {
		return "", err
	}
	for _, candidate := range candidates {
		if candidate.ForkedFromID != sourceThreadID {
			return "", errors.New("existing Codex thread does not match the requested conversation fork")
		}
		if err := c.RequireThreadTurnsIdle(ctx, candidate.ID); err != nil {
			return "", err
		}
		return c.ResumeThreadWithSettings(ctx, candidate.ID, cwd, environment, settings)
	}
	if options.RetainedRoster {
		return "", errors.New("retained team roster prevents root thread replacement")
	}
	if options.BeforeCreate != nil {
		options.BeforeCreate()
	}
	return c.ForkThread(ctx, sourceThreadID, cwd, environment, settings)
}

func recoveryIdentityEqual(left, right codex.ThreadMetadata) bool {
	project := func(value *string) string {
		if value == nil {
			return ""
		}
		return *value
	}
	return left.ID == right.ID && left.Cwd == right.Cwd &&
		workspaceThread(left.Source) == workspaceThread(right.Source) &&
		left.ForkedFromID == right.ForkedFromID && project(left.ProjectID) == project(right.ProjectID)
}

func (c *Client) creationCandidates(ctx context.Context, recordedID, cwd string, options RecoveryOptions) (map[string]codex.ThreadMetadata, error) {
	candidates := map[string]codex.ThreadMetadata{}
	var refusal error
	ambiguous := func() error {
		return fmt.Errorf("multiple Codex threads use creation directory %s; refusing ambiguous recovery", cwd)
	}
	merge := func(candidate codex.ThreadMetadata) error {
		if candidate.ID == "" {
			return errors.New("thread discovery returned an empty identity")
		}
		if project, excluded := options.ExcludedThreads[candidate.ID]; excluded {
			if candidate.Cwd != cwd || (project != "" && (candidate.ProjectID == nil || *candidate.ProjectID != project)) {
				refusal = errors.New("retained member thread has the wrong cwd or project identity")
				return refusal
			}
			return nil
		}
		if candidate.Cwd != cwd || !workspaceThread(candidate.Source) {
			return errors.New("thread discovery returned an invalid creation candidate")
		}
		if previous, exists := candidates[candidate.ID]; exists && !recoveryIdentityEqual(previous, candidate) {
			refusal = errors.New("thread discovery returned contradictory creation identities")
			return refusal
		}
		candidates[candidate.ID] = candidate
		if len(candidates) > 1 {
			return ambiguous()
		}
		return nil
	}
	finish := creationprogress.Begin(options.Progress, "recovery_loaded", "")
	loaded, err := c.LoadedThreadIDs(ctx)
	if err != nil {
		return nil, err
	}
	for _, loadedID := range loaded {
		metadata, err := c.ReadThreadMetadata(ctx, loadedID, false)
		if err != nil {
			current, listErr := c.LoadedThreadIDs(ctx)
			if listErr == nil && !slices.Contains(current, loadedID) {
				continue
			}
			return nil, err
		}
		if loadedID == recordedID && (metadata.Cwd != cwd || !workspaceThread(metadata.Source)) {
			return nil, errors.New("recorded creation thread has the wrong identity")
		}
		_, member := options.ExcludedThreads[loadedID]
		if member || (metadata.Cwd == cwd && workspaceThread(metadata.Source)) {
			if err := merge(metadata); err != nil {
				return nil, err
			}
		}
	}
	finish()
	scan := func(indexed bool) error {
		stage := "recovery_scan"
		if indexed {
			stage = "recovery_index"
		}
		finish := creationprogress.Begin(options.Progress, stage, "")
		seenCursors := map[string]bool{}
		seenRows := map[string]codex.ThreadMetadata{}
		cursor := ""
		rows := 0
		archived := false
		for page := 0; page < 64; page++ {
			listed, next, err := c.ListThreads(ctx, codex.ThreadListOptions{
				Cwd: cwd, SourceKinds: []string{threadSourceKind}, Archived: &archived,
				Limit: 100, SortDirection: "asc", Cursor: cursor, UseStateDBOnly: indexed,
			})
			if err != nil {
				return err
			}
			rows += len(listed)
			if rows > 4096 {
				return errors.New("thread discovery exceeded its row bound")
			}
			for _, row := range listed {
				if row.ID == "" || row.Cwd != cwd || !workspaceThread(row.Source) {
					return errors.New("thread/list returned an invalid creation candidate")
				}
				if previous, exists := seenRows[row.ID]; exists {
					if !recoveryIdentityEqual(previous, row) {
						return errors.New("thread/list returned contradictory creation rows")
					}
					continue
				}
				seenRows[row.ID] = row
				candidate := row
				if indexed {
					candidate, err = c.ReadThreadMetadata(ctx, row.ID, false)
					if err != nil {
						return err
					}
					if candidate.ID == recordedID && (candidate.Cwd != cwd || !workspaceThread(candidate.Source)) {
						refusal = errors.New("recorded creation thread has the wrong identity")
						return refusal
					}
					if project, member := options.ExcludedThreads[candidate.ID]; member && (candidate.Cwd != cwd || (project != "" && (candidate.ProjectID == nil || *candidate.ProjectID != project))) {
						refusal = errors.New("retained member thread has the wrong cwd or project identity")
						return refusal
					}
					if !recoveryIdentityEqual(row, candidate) {
						return errors.New("thread index returned stale creation identity")
					}
				}
				if err := merge(candidate); err != nil {
					return err
				}
			}
			if next == nil || *next == "" {
				finish()
				return nil
			}
			if seenCursors[*next] {
				return errors.New("thread/list repeated a creation cursor")
			}
			seenCursors[*next] = true
			cursor = *next
		}
		return errors.New("thread discovery exceeded its page bound")
	}
	// The index can prove ambiguity early, but even a positive page cannot prove
	// uniqueness. Every adoption or replacement still requires complete discovery.
	_ = scan(true)
	if refusal != nil {
		return nil, refusal
	}
	if len(candidates) > 1 {
		return nil, ambiguous()
	}
	if err := scan(false); err != nil {
		return nil, err
	}
	return candidates, nil
}

func (c *Client) ResolveForkSettings(
	ctx context.Context, sourceThreadID string, requested codex.ThreadSettings, modelProfile, provider string,
) (codex.ThreadSettings, error) {
	if sourceThreadID == "" {
		return codex.ThreadSettings{}, errors.New("fork settings require a source thread")
	}
	source, err := c.ReadThreadSettings(ctx, sourceThreadID)
	if err != nil {
		return codex.ThreadSettings{}, fmt.Errorf("read source Codex settings: %w", err)
	}
	var models []codex.Model
	models, err = c.ModelsForDirectory(ctx, "", modelProfile, provider)
	if err != nil {
		return codex.ThreadSettings{}, fmt.Errorf("load Codex models: %w", err)
	}
	return codex.ResolveForkThreadSettings(models, source, requested)
}

func (c *Client) RecoverArchivedThread(
	ctx context.Context, threadID, cwd string, environment map[string]string,
) (string, error) {
	if threadID == "" || cwd == "" {
		return "", errors.New("archived thread recovery requires a thread id and working directory")
	}
	active, activeFound, err := c.retirementCandidate(ctx, cwd, false, false)
	if err != nil {
		return "", err
	}
	_, archivedFound, err := c.retirementThreadByID(ctx, threadID, cwd, true)
	if err != nil {
		return "", err
	}
	if activeFound && active.ID != threadID {
		return "", errors.New("another active Codex thread uses the revived session directory")
	}
	if activeFound && archivedFound {
		return "", errors.New("the revived Codex thread exists in both active and archived history")
	}
	if activeFound {
		return c.ResumeThread(ctx, threadID, cwd, environment)
	}
	if !archivedFound {
		return "", errors.New("the revived Codex thread is neither active nor archived")
	}
	thread, err := c.UnarchiveThread(ctx, threadID)
	if err != nil {
		return "", err
	}
	if thread.Cwd != cwd || !workspaceThread(thread.Source) {
		return "", errors.New("thread/unarchive returned the wrong Codex thread identity")
	}
	return c.ResumeThread(ctx, threadID, cwd, environment)
}

func (c *Client) RetireThread(ctx context.Context, threadID, cwd string, force bool) (retireErr error) {
	stage := "find session conversation"
	defer func() {
		if retireErr != nil {
			retireErr = fmt.Errorf("%s: %w", stage, retireErr)
		}
	}()
	var candidate codex.ThreadMetadata
	var found bool
	var err error
	if threadID == "" {
		threadID, err = c.ThreadOperationAttempt(cwd)
		if err != nil {
			return err
		}
		if threadID == "" {
			candidate, found, err = c.retirementCandidate(ctx, cwd, false, false)
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
			threadID = candidate.ID
			if err := c.RecordThreadOperationAttempt(cwd, threadID); err != nil {
				return fmt.Errorf("record Codex thread retirement before archival: %w", err)
			}
		}
	}
	stage = "prove session conversation archive state"
	archiveState, err := c.ProveArchivedRootThread(ctx, threadID, cwd)
	if err != nil {
		return err
	}
	stage = "find session conversation"
	candidate, found, err = c.retirementCandidate(ctx, cwd, false, true)
	if err != nil {
		return err
	}
	if archiveState == ArchiveArchived && found {
		return errors.New("archived Codex thread also appears in active discovery")
	}
	if found && candidate.ID != threadID {
		return errors.New("another Codex thread uses the portal session directory")
	}
	if archiveState == ArchiveArchived {
		stage = "clear retired conversation attempts"
		return c.ClearConversationAttempts(threadID, cwd)
	}
	loadedDiscovery := !found && (archiveState == ArchiveFresh || archiveState == ArchiveActive)
	if loadedDiscovery {
		candidate, found, err = loadedRetirementCandidate(ctx, c, threadID, cwd)
		if err != nil {
			return err
		}
	}
	var persistedFile os.FileInfo
	if !found {
		stage = "prove persisted empty-preview session conversation"
		candidate, persistedFile, err = c.persistedRetirementCandidate(ctx, threadID, cwd)
		if err != nil {
			return err
		}
	}
	stage = "verify session conversation"
	metadata, err := c.ReadThreadMetadata(ctx, threadID, persistedFile == nil)
	if err != nil {
		return err
	}
	if metadata.Cwd != cwd || !workspaceThread(metadata.Source) ||
		((loadedDiscovery || persistedFile != nil) && !reflect.DeepEqual(candidate, metadata)) {
		return errors.New("Codex thread does not match the portal session being removed")
	}
	materialized, err := c.HistoryMaterialized(ctx, threadID, cwd)
	if err != nil {
		return err
	}
	if persistedFile != nil && !materialized {
		return errors.New("persisted session conversation has no materialized history")
	}
	stage = "interrupt session conversation"
	if force && materialized {
		turnID, err := c.ActiveTurnID(ctx, threadID)
		if err != nil {
			return err
		}
		if turnID != "" {
			if err := c.Interrupt(ctx, threadID); err != nil {
				return err
			}
		}
	}
	stage = "verify conversation is idle"
	for materialized {
		err := c.RequireThreadTurnsIdle(ctx, threadID)
		if err == nil {
			break
		}
		if !force {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for interrupted Codex thread: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if persistedFile != nil {
		stage = "verify persisted conversation submissions"
		if err := c.RequireSubmissionAttemptsResolved(ctx, threadID); err != nil {
			return err
		}
		stage = "recheck persisted empty-preview session conversation"
		if err := c.provePersistedRetirementCandidate(ctx, candidate, cwd, persistedFile); err != nil {
			return err
		}
	}
	stage = "archive session conversation"
	if err := c.ArchiveThread(ctx, threadID); err != nil {
		return err
	}
	stage = "clear retired conversation attempts"
	return c.ClearConversationAttempts(threadID, cwd)
}

type ThreadActivity struct {
	ID        string
	Cwd       string
	UpdatedAt time.Time
}

func (c *Client) ListThreadActivity(
	ctx context.Context, expected []ThreadActivity,
) ([]ThreadActivity, error) {
	activities := make([]ThreadActivity, 0, len(expected))
	for _, identity := range expected {
		thread, err := c.ReadThreadMetadata(ctx, identity.ID, true)
		if err != nil {
			return nil, err
		}
		if thread.Cwd != identity.Cwd || thread.UpdatedAt < 0 || !workspaceThread(thread.Source) {
			return nil, errors.New("thread/read returned invalid activity metadata")
		}
		activities = append(activities, ThreadActivity{
			ID: thread.ID, Cwd: thread.Cwd, UpdatedAt: time.Unix(thread.UpdatedAt, 0),
		})
	}
	return activities, nil
}

func (c *Client) RequireThreadMaterialized(ctx context.Context, threadID, cwd string) error {
	materialized, err := c.HistoryMaterialized(ctx, threadID, cwd)
	if err != nil {
		return err
	}
	if !materialized {
		return errors.New("recorded Codex thread has no persisted history; archive the session and start a new one")
	}
	return nil
}

func (c *Client) retirementCandidate(
	ctx context.Context, cwd string, archived, indexed bool,
) (codex.ThreadMetadata, bool, error) {
	threads, next, err := c.retirementThreads(ctx, cwd, archived, indexed, 2, "")
	if err != nil {
		return codex.ThreadMetadata{}, false, err
	}
	if len(threads) > 1 || next != nil {
		return codex.ThreadMetadata{}, false,
			fmt.Errorf("multiple Codex threads use %s; refusing ambiguous retirement", cwd)
	}
	if len(threads) == 0 {
		return codex.ThreadMetadata{}, false, nil
	}
	candidate := threads[0]
	if candidate.ID == "" || candidate.Cwd != cwd || !workspaceThread(candidate.Source) {
		return codex.ThreadMetadata{}, false, errors.New("thread/list returned an invalid retirement candidate")
	}
	return candidate, true, nil
}

// Empty discovery can omit a persisted root with no preview. Only a stable,
// materialized original vscode identity can supply the missing candidate.
func (c *Client) persistedRetirementCandidate(ctx context.Context, threadID, cwd string) (codex.ThreadMetadata, os.FileInfo, error) {
	metadata, err := c.ReadThreadMetadata(ctx, threadID, false)
	if err != nil {
		return codex.ThreadMetadata{}, nil, fmt.Errorf("read persisted retirement metadata: %w", err)
	}
	if err := validateArchiveMetadata(metadata, ArchivedThreadIdentity{
		ThreadID: threadID, Cwd: cwd, SourceKind: threadSourceKind,
	}); err != nil {
		return codex.ThreadMetadata{}, nil, err
	}
	if metadata.Ephemeral == nil || *metadata.Ephemeral {
		return codex.ThreadMetadata{}, nil, errors.New("persisted retirement root is not explicitly non-ephemeral")
	}
	if metadata.Preview != "" {
		return codex.ThreadMetadata{}, nil, errors.New("persisted retirement root does not have an empty preview")
	}
	if len(metadata.Status) != 1 || metadata.Status["type"] != "notLoaded" {
		return codex.ThreadMetadata{}, nil, errors.New("persisted retirement root is not positively unloaded")
	}
	if metadata.Path == nil {
		return codex.ThreadMetadata{}, nil, errors.New("persisted retirement root has no rollout path")
	}
	file, err := os.Lstat(*metadata.Path)
	if err != nil || !file.Mode().IsRegular() {
		return codex.ThreadMetadata{}, nil, errors.New("persisted retirement rollout is not a materialized regular file")
	}
	if err := c.provePersistedRetirementCandidate(ctx, metadata, cwd, file); err != nil {
		return codex.ThreadMetadata{}, nil, err
	}
	return metadata, file, nil
}

func (c *Client) provePersistedRetirementCandidate(ctx context.Context, metadata codex.ThreadMetadata, cwd string, before os.FileInfo) error {
	state, err := ProveArchivedThread(ctx, archiveIdleMetadataReader{c.Client, metadata}, ArchivedThreadIdentity{
		ThreadID: metadata.ID, Cwd: cwd, CodexHome: c.CodexHome, SourceKind: threadSourceKind,
		RequireActiveFile: true,
	})
	if err != nil {
		return fmt.Errorf("prove materialized retirement root: %w", err)
	}
	if state != ArchiveActive {
		return errors.New("persisted retirement root is no longer positively active")
	}
	after, err := os.Lstat(*metadata.Path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("persisted retirement rollout changed during proof")
	}
	return ctx.Err()
}

type loadedRetirementReader interface {
	ThreadMetadataReader
	LoadedThreadIDs(context.Context) ([]string, error)
}

// An empty index can omit a loaded root. Exact archive proof still owns the
// root identity; loaded metadata must prove it is the only matching thread.
func loadedRetirementCandidate(ctx context.Context, c loadedRetirementReader, threadID, cwd string) (codex.ThreadMetadata, bool, error) {
	loaded, err := c.LoadedThreadIDs(ctx)
	if err != nil {
		// A canceled read can race a disconnect. Preserve both causes without
		// changing successful observations or any mutation receipt handling.
		if cause := ctx.Err(); cause != nil {
			err = errors.Join(err, cause)
		}
		return codex.ThreadMetadata{}, false, err
	}
	var candidate codex.ThreadMetadata
	found := false
	for _, id := range loaded {
		metadata, err := c.ReadThreadMetadata(ctx, id, true)
		if err != nil {
			if cause := ctx.Err(); cause != nil {
				err = errors.Join(err, cause)
			}
			return codex.ThreadMetadata{}, false, err
		}
		if !canonicalArchivePath(metadata.Cwd) || metadata.Source == nil {
			return codex.ThreadMetadata{}, false, errors.New("thread/read returned invalid retirement metadata")
		}
		if id == threadID {
			if metadata.Cwd != cwd || !workspaceThread(metadata.Source) ||
				(found && !reflect.DeepEqual(candidate, metadata)) {
				return codex.ThreadMetadata{}, false, errors.New("Codex thread does not match the portal session being removed")
			}
			candidate, found = metadata, true
		} else if metadata.Cwd == cwd && workspaceThread(metadata.Source) {
			return codex.ThreadMetadata{}, false, errors.New("another Codex thread uses the portal session directory")
		}
	}
	if err := ctx.Err(); err != nil {
		return codex.ThreadMetadata{}, false, err
	}
	return candidate, found, nil
}

func (c *Client) retirementThreadByID(
	ctx context.Context, threadID, cwd string, archived bool,
) (codex.ThreadMetadata, bool, error) {
	seenCursors := make(map[string]struct{})
	var cursor string
	for {
		threads, next, err := c.retirementThreads(ctx, cwd, archived, false, 100, cursor)
		if err != nil {
			return codex.ThreadMetadata{}, false, err
		}
		for _, candidate := range threads {
			if candidate.ID != threadID {
				continue
			}
			if candidate.Cwd != cwd || !workspaceThread(candidate.Source) {
				return codex.ThreadMetadata{}, false,
					errors.New("thread/list returned invalid metadata for the expected retirement thread")
			}
			return candidate, true, nil
		}
		if next == nil || *next == "" {
			return codex.ThreadMetadata{}, false, nil
		}
		cursor = *next
		if _, duplicate := seenCursors[cursor]; duplicate {
			return codex.ThreadMetadata{}, false, errors.New("thread/list repeated a retirement cursor")
		}
		seenCursors[cursor] = struct{}{}
	}
}

// The pinned shared list options cannot express the native all-provider filter.
// Retirement must see both the recorded root and conflicting directory users
// even when their provider differs from the daemon's default. An explicit empty
// modelProviders array means all providers; omission uses the native default.
func (c *Client) retirementThreads(
	ctx context.Context, cwd string, archived, indexed bool, limit int, cursor string,
) ([]codex.ThreadMetadata, *string, error) {
	params := map[string]any{
		"cwd": cwd, "sourceKinds": []string{threadSourceKind}, "archived": archived,
		"limit": limit, "sortDirection": "asc", "modelProviders": []string{},
	}
	if indexed {
		params["useStateDbOnly"] = true
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	var page struct {
		Data       *[]codex.ThreadMetadata `json:"data"`
		NextCursor *string                 `json:"nextCursor"`
	}
	if err := c.Request(ctx, "thread/list", params, &page); err != nil {
		return nil, nil, err
	}
	if page.Data == nil {
		return nil, nil, errors.New("thread/list returned no data")
	}
	return *page.Data, page.NextCursor, nil
}

func workspaceThread(source any) bool {
	value, ok := source.(string)
	return ok && value == threadSourceKind
}
