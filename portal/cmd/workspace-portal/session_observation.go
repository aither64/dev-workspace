package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

func sessionCommand(args []string) error {
	if len(args) == 0 || args[0] != "observe" {
		return errors.New("usage: workspace-portal session observe")
	}
	flags := flag.NewFlagSet("session observe", flag.ContinueOnError)
	workspace := flags.String("workspace", "", "host-selected workspace")
	slug := flags.String("session-slug", "", "session slug")
	socket := flags.String("socket", "", "selected App Server socket")
	stateRoot := flags.String("user-state-root", "", "selected private user state root")
	authorityDir := flags.String("authority-dir", "", "selected runtime authority directory")
	codexHome := flags.String("codex-home", "", "selected Codex home")
	expectedID := flags.String("expected-operation-id", "", "validated executing archive operation")
	expectedMode := flags.String("expected-archive-mode", "", "validated executing archive mode")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || !session.ValidSlug(*slug) || !canonicalAbsolutePath(*workspace) ||
		!canonicalAbsolutePath(*socket) || !canonicalAbsolutePath(*stateRoot) ||
		!canonicalAbsolutePath(*authorityDir) || !canonicalAbsolutePath(*codexHome) ||
		os.Getenv("DEV_WORKSPACE_CODEX_HOME") != *codexHome {
		return errors.New("session observe requires complete host-selected runtime coordinates")
	}
	if err := portalweb.ValidateObservationArchiveContext(*expectedID, *expectedMode); err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(*workspace)
	if err != nil || canonical != *workspace {
		return errors.New("session observe requires a canonical workspace")
	}
	summary, err := session.Find(*workspace, *slug)
	if err != nil {
		return err
	}
	if summary.Archived && *expectedID == "" {
		return errors.New("semantic inactivity observation requires active tracking")
	}
	if summary.Archived && summary.Codex.ThreadID != "" {
		return errors.New("retained inactivity observation requires active tracking")
	}
	directory := filepath.Join(*workspace, summary.Root, *slug)
	identity, err := session.ObservationIdentity(*workspace, *slug, directory, summary.Codex.ThreadID)
	if err != nil {
		return err
	}
	result := teamruntime.Observation{Schema: 1, Workspace: *workspace, Slug: *slug, Identity: identity,
		Subjects: []teamruntime.ObservationSubject{}, Diagnostics: []workspacecodex.ObservationDiagnostic{}}
	store, err := teamruntime.NewStore(*stateRoot, *workspace)
	if err != nil {
		return err
	}
	client := newCodexClient(*socket, *workspace)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if summary.Codex.ThreadID == "" {
		check := func() error {
			current, err := session.Find(*workspace, *slug)
			if err != nil || current.Root != summary.Root {
				return errors.New("tracking location changed")
			}
			if err := session.RequireNormalizedThreadless(current); err != nil {
				return err
			}
			after, err := session.ObservationIdentity(*workspace, *slug, directory, current.Codex.ThreadID)
			if err != nil || after != identity {
				return errors.New("tracking identity changed")
			}
			if err := store.RequireNoRoster(*slug); err != nil {
				return err
			}
			if err := session.RequireNoRuntime(ctx, *workspace, *slug, *authorityDir, os.Getenv("DEV_SESSION_TMUX_SOCKET")); err != nil {
				return err
			}
			return portalweb.RequireObservationReceipts(current, *stateRoot, *expectedID, *expectedMode)
		}
		if err := check(); err != nil {
			result.Unknown("absence_unverified", "Threadless tracking or operation absence cannot be verified.")
		} else if err := workspacecodex.RequireThreadlessConversations(ctx, client, filepath.Join(*workspace, "work", *slug)); err != nil {
			code := "absence_unverified"
			var failure *workspacecodex.ObservationError
			if errors.As(err, &failure) {
				code = failure.Code
			}
			result.Unknown(code, "Threadless conversation or submission absence cannot be verified.")
		} else if err := check(); err != nil {
			result.Unknown("identity_changed", "Threadless tracking or operation changed during observation.")
		} else {
			result.ActivityKnown, result.Idle = true, true
			result.ActivityToken = workspacecodex.ObservationToken([]any{1, "verified-threadless"})
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if summary.Creation.State != "ready" || summary.Codex.SocketPath != *socket {
		result.Unknown("creation_unverified", "Retained conversation creation or selected socket cannot be verified.")
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	authority, err := session.LoadRuntimeAuthority(*authorityDir, *slug, *workspace)
	if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil &&
		(authority.State != "ready" || authority.CodexThreadID != summary.Codex.ThreadID || authority.CodexSocketPath != *socket) {
		result.Unknown("authority_unverified", "Retained runtime authority cannot be verified.")
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	service := teamruntime.Service{Store: store, Client: client, Workspace: *workspace}
	result, err = service.ObserveRetained(ctx, *slug, summary.Codex.ThreadID, identity)
	if err != nil {
		return err
	}
	again, err := session.Find(*workspace, *slug)
	if err != nil || again.Codex.ThreadID != summary.Codex.ThreadID || again.Root != summary.Root {
		result.Unknown("identity_changed", "Retained tracking identity changed during observation.")
	} else if after, identityErr := session.ObservationIdentity(*workspace, *slug, directory, again.Codex.ThreadID); identityErr != nil || after != identity {
		result.Unknown("identity_changed", "Tracking directory changed during observation.")
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
