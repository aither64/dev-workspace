package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

const monitorRequestLimit = 128 * 1024

type monitorRequest struct {
	Action     string
	ID         string
	Start      monitoroperation.StartRequest
	Report     monitoroperation.ReportRequest
	Resolution monitoroperation.ResolutionRequest
}

// Decode each action independently: report cannot carry an operation selector,
// and status/cancel cannot smuggle launch or outcome fields.
func decodeMonitorRequest(raw []byte) (monitorRequest, error) {
	var header struct {
		Action string `json:"action"`
	}
	if len(raw) > monitorRequestLimit || json.Unmarshal(raw, &header) != nil {
		return monitorRequest{}, errors.New("invalid monitor operation request")
	}
	request := monitorRequest{Action: header.Action}
	switch header.Action {
	case "start":
		var value struct {
			Action string `json:"action"`
			monitoroperation.StartRequest
		}
		if err := teamMCPDecode(raw, &value); err != nil {
			return request, err
		}
		request.Start = value.StartRequest
	case "report":
		var value struct {
			Action string `json:"action"`
			monitoroperation.ReportRequest
		}
		if err := teamMCPDecode(raw, &value); err != nil {
			return request, err
		}
		request.Report = value.ReportRequest
	case "resolve":
		var value struct {
			Action string `json:"action"`
			monitoroperation.ResolutionRequest
		}
		if err := teamMCPDecode(raw, &value); err != nil {
			return request, err
		}
		request.Resolution = value.ResolutionRequest
	case "status", "cancel":
		var value struct {
			Action string `json:"action"`
			ID     string `json:"id"`
		}
		if err := teamMCPDecode(raw, &value); err != nil ||
			(value.ID != "" && !teamMCPMessageIDPattern.MatchString(value.ID)) || header.Action == "cancel" && value.ID == "" {
			return request, errors.New("monitor status/cancel requires an exact operation ID")
		}
		request.ID = value.ID
	default:
		return request, errors.New("unknown monitor operation action")
	}
	return request, nil
}

func monitorCommand(args []string) error {
	flags := flag.NewFlagSet("monitor", flag.ContinueOnError)
	workspace := flags.String("workspace", "", "canonical workspace")
	state := flags.String("user-state-root", "", "private state root")
	authorityDir := flags.String("authority-dir", "", "runtime authority directory")
	socket := flags.String("socket", "", "Codex socket")
	slug := flags.String("session-slug", "", "owning session")
	caller := flags.String("expected-caller-thread-id", "", "native monitor caller")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !canonicalAbsolutePath(*workspace) || !canonicalAbsolutePath(*state) ||
		!canonicalAbsolutePath(*authorityDir) || !canonicalAbsolutePath(*socket) ||
		!session.ValidSlug(*slug) || !teamMCPThreadIDPattern.MatchString(*caller) {
		return errors.New("monitor requires exact workspace, private state, authority, socket, session and native caller")
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, monitorRequestLimit+1))
	if err != nil {
		return err
	}
	request, err := decodeMonitorRequest(raw)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	client := newCodexClient(*socket, *workspace, *state)
	defer client.Close()
	service, err := newMonitorService(*workspace, *state, *authorityDir, *socket,
		filepath.Dir(filepath.Dir(executable)), client)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := monitorCaller(ctx, service.Store, *workspace, *caller, request)
	if err != nil || owner.Slug != *slug {
		return errors.New("monitor caller does not own this session")
	}
	if request.Action == "status" || request.Action == "resolve" {
		id := request.ID
		if request.Action == "resolve" {
			id = request.Resolution.OperationID
		}
		if id != "" {
			op, err := service.Store.Get(ctx, id)
			if err != nil {
				return err
			}
			if *caller == op.Owner.RootThreadID {
				// These callbacks admit only metadata reconciliation for this exact
				// request, including its original pending lifecycle owner. They do
				// not replace the ordinary start/report authority callbacks.
				service.ResolveLead = func(_ context.Context, thread string) (monitoroperation.Owner, error) {
					return monitorRecoveryOwner(*workspace, op, thread)
				}
				service.Resolve = func(slug string) (monitoroperation.Owner, error) {
					if slug != op.Owner.Slug {
						return monitoroperation.Owner{}, errors.New("monitor recovery session changed")
					}
					return monitorRecoveryOwner(*workspace, op, *caller)
				}
			}
			if *caller != op.Owner.RootThreadID && *caller != op.Launch.ThreadID {
				return errors.New("monitor operation is not owned by this caller")
			}
			if err := reconcileMonitorReceipts(ctx, service, client, id, *caller); err != nil {
				return err
			}
		}
	}
	var result monitoroperation.Operation
	switch request.Action {
	case "start":
		result, err = service.Start(ctx, *caller, request.Start)
	case "report":
		result, err = service.Report(ctx, *caller, request.Report)
	case "status":
		result, err = service.Status(ctx, *caller, request.ID)
	case "cancel":
		result, err = service.Cancel(ctx, *caller, request.ID)
	case "resolve":
		result, err = service.ResolveEffects(ctx, *caller, request.Resolution)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

// The existing Ruby lifecycle owner calls this while retaining its exclusive
// session lock. Native tools cannot request these internal lifecycle actions.
func monitorLifecycleCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("monitor lifecycle action is required")
	}
	action := args[0]
	flags := flag.NewFlagSet("monitor-lifecycle", flag.ContinueOnError)
	workspace := flags.String("workspace", "", "canonical workspace")
	stateRoot := flags.String("user-state-root", "", "private state root")
	slug := flags.String("session-slug", "", "owning session")
	root := flags.String("root-thread-id", "", "retained lead")
	socket := flags.String("socket", "", "Codex socket")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || !canonicalAbsolutePath(*workspace) || !canonicalAbsolutePath(*stateRoot) ||
		!session.ValidSlug(*slug) || (*root != "" && (!canonicalAbsolutePath(*socket) || !teamMCPThreadIDPattern.MatchString(*root))) {
		return errors.New("monitor lifecycle requires exact workspace, state, session, lead and socket")
	}
	if action == "archive" || action == "revoke" {
		pending, err := session.PendingLifecycle(*workspace, *slug)
		if err != nil || (pending != "archive" && pending != "delete") {
			return errors.New("monitor retirement requires the original archive/delete journal owner")
		}
	}
	store, err := monitoroperation.NewStore(*stateRoot, *workspace)
	if err != nil {
		return err
	}
	if *root == "" {
		operations, err := store.List(context.Background(), *slug)
		if err != nil || len(operations) != 0 {
			return errors.Join(err, errors.New("threadless tracking contradicts retained monitor ownership"))
		}
		return nil
	}
	construct := newCodexClient
	if action == "require-execution-idle" {
		construct = newTransitionClient
	}
	client := construct(*socket, *workspace, *stateRoot)
	defer client.Close()
	lifecycle := monitoroperation.Lifecycle{Store: store, Client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch action {
	case "require-idle":
		return lifecycle.RequireIdle(ctx, *slug, *root, false)
	case "require-execution-idle":
		return lifecycle.RequireIdle(ctx, *slug, *root, true)
	case "require-archived":
		return lifecycle.RequireArchived(ctx, *slug, *root)
	case "revoke":
		return lifecycle.Revoke(ctx, *slug, *root)
	case "archive":
		return lifecycle.Archive(ctx, *slug, *root)
	default:
		return errors.New("unknown monitor lifecycle action")
	}
}

func newMonitorService(workspace, state, authorityDir, socket, packageRoot string, client *workspacecodex.Client) (*monitoroperation.Service, error) {
	store, err := monitoroperation.NewStore(state, workspace)
	if err != nil {
		return nil, err
	}
	authority := monitoroperation.NativeAuthority{
		Workspace: workspace, StateRoot: state, AuthorityDir: authorityDir, Socket: socket, Client: client.Client,
		Admission: func(owner monitoroperation.Owner) error {
			provider, err := monitorProviderState(workspace, state, socket)
			if err != nil {
				return err
			}
			if provider != nil && provider.BlocksSession(owner.Slug, owner.RootThreadID) {
				return errors.New("monitor owner provider transition is reserved")
			}
			return nil
		},
		Provider: func(slug string) (string, error) {
			provider, err := monitorProviderState(workspace, state, socket)
			if err != nil {
				return "", err
			}
			summary, err := session.Find(workspace, slug)
			if err != nil {
				return "", err
			}
			if provider != nil {
				return provider.Provider(slug, summary.Codex.ThreadID), nil
			}
			// The reusable settings projection omits modelProvider. Read only
			// that required native metadata field; never infer a default name.
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			var metadata struct {
				Thread struct {
					ID, Cwd, ModelProvider string
				} `json:"thread"`
			}
			if err := client.Client.Request(ctx, "thread/read", map[string]any{"threadId": summary.Codex.ThreadID, "excludeTurns": true}, &metadata); err != nil {
				return "", err
			}
			if metadata.Thread.ID != summary.Codex.ThreadID || metadata.Thread.Cwd != filepath.Join(workspace, "work", slug) || metadata.Thread.ModelProvider == "" {
				return "", errors.New("monitor effective native provider cannot be verified")
			}
			return metadata.Thread.ModelProvider, nil
		},
	}
	return &monitoroperation.Service{Store: store, Client: client.Client,
		ResolveLead: authority.ResolveLead, Resolve: authority.Resolve,
		LockSession: authority.LockSession, Admit: authority.Admit, Activate: authority.Activate,
		CheckResolution: (monitoroperation.Lifecycle{Store: store, Client: client}).ProveQuiescent,
		Policy: func(context.Context, monitoroperation.Owner) (monitoroperation.UtilityPolicy, error) {
			installed, err := agentteams.LoadInstalled(packageRoot)
			if err != nil {
				return monitoroperation.UtilityPolicy{}, err
			}
			if !installed.Managed || installed.Catalog == nil {
				return monitoroperation.UtilityPolicy{}, errors.New("monitor requires an installed verification utility policy")
			}
			metadata := installed.Metadata.AgentTeams
			pin := agentteams.CatalogPin{PackagePath: packageRoot, CatalogPath: metadata.Catalog.Path,
				CatalogDigest: metadata.Catalog.Digest, SchemaVersion: metadata.Catalog.SchemaVersion,
				MetadataSchema: metadata.MetadataSchema, Generator: metadata.Generator,
				NativeAdapter: metadata.NativeAdapter, Capacity: metadata.Capacity}
			retained, err := agentteams.LoadPinned(packageRoot, pin)
			if err != nil {
				return monitoroperation.UtilityPolicy{}, err
			}
			return monitoroperation.PolicyFromCatalog(packageRoot, &retained, installed.Catalog)
		}}, nil
}

func monitorProviderState(workspace, stateRoot, socket string) (*codexproviders.State, error) {
	name := os.Getenv("DEV_WORKSPACE_NAME")
	if name == "" {
		return nil, nil
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if errors.Is(err, os.ErrNotExist) {
		path, pathErr := codexproviders.StatePath(stateRoot, name)
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
	state, err := codexproviders.ReadState(stateRoot, name, workspace, socket, catalog)
	return &state, err
}

func monitorCaller(ctx context.Context, store *monitoroperation.Store, workspace, thread string, request monitorRequest) (monitoroperation.Owner, error) {
	if request.Action != "report" {
		if owner, err := monitoroperation.ResolveLead(workspace, thread); err == nil {
			return owner, nil
		} else if request.Action == "status" || request.Action == "resolve" {
			id := request.ID
			if request.Action == "resolve" {
				id = request.Resolution.OperationID
			}
			if id != "" {
				if op, err := store.Get(ctx, id); err == nil && op.Owner.RootThreadID == thread {
					return monitorRecoveryOwner(workspace, op, thread)
				}
			}
		} else {
			return monitoroperation.Owner{}, err
		}
	}
	if request.Action != "report" && request.Action != "status" {
		return monitoroperation.Owner{}, errors.New("monitor action requires its exact current lead")
	}
	operation, err := store.ResolveReporter(ctx, thread)
	return operation.Owner, err
}

func monitorRecoveryOwner(workspace string, op monitoroperation.Operation, thread string) (monitoroperation.Owner, error) {
	if thread != op.Owner.RootThreadID || op.Workspace != workspace {
		return monitoroperation.Owner{}, errors.New("monitor recovery caller changed")
	}
	summary, err := session.Find(workspace, op.Owner.Slug)
	if err != nil || summary.Codex.ThreadID != thread || summary.Codex.SocketPath != op.Owner.Socket || summary.Creation.State == "creating" {
		return monitoroperation.Owner{}, errors.New("monitor recovery retained owner changed")
	}
	incarnation, err := session.ObservationIdentity(workspace, op.Owner.Slug, filepath.Join(workspace, summary.Root, summary.Slug), thread)
	if err != nil || incarnation != op.Owner.Incarnation {
		return monitoroperation.Owner{}, errors.New("monitor recovery incarnation changed")
	}
	progress, err := session.PendingLifecycleProgress(workspace, op.Owner.Slug)
	if err != nil {
		return monitoroperation.Owner{}, err
	}
	if progress != nil {
		if progress.Operation != "archive" && progress.Operation != "delete" ||
			progress.RootRecorded && (progress.RetainedThreadID == nil || *progress.RetainedThreadID != thread) {
			return monitoroperation.Owner{}, errors.New("monitor recovery lifecycle owner changed")
		}
	} else if summary.Archived || summary.Terminal {
		return monitoroperation.Owner{}, errors.New("monitor recovery session is retired")
	}
	owner := op.Owner
	if epoch, err := session.SocketIdentity(owner.Socket); err == nil {
		owner.SocketEpoch = epoch
	}
	return owner, nil
}

func reconcileMonitorReceipts(ctx context.Context, service *monitoroperation.Service, client *workspacecodex.Client, id, caller string) error {
	op, err := service.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	unlock, err := service.LockSession(ctx, op.Owner.Slug)
	if err != nil {
		return err
	}
	defer unlock()
	tx, err := service.Store.Lock(ctx, id)
	if err != nil {
		return err
	}
	defer tx.Close()
	if caller != tx.Operation.Owner.RootThreadID && caller != tx.Operation.Launch.ThreadID {
		return errors.New("monitor receipt recovery caller changed")
	}
	if _, err := monitorRecoveryOwner(tx.Operation.Workspace, *tx.Operation, tx.Operation.Owner.RootThreadID); err != nil {
		return err
	}
	// Keep every recovered receipt even when another original send is unknown.
	_ = monitoroperation.ReconcileMonitorEvents(ctx, tx.Operation, monitoroperation.NativeDelivery{Client: client})
	return tx.Save()
}
