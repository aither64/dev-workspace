package web

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type piRuntimeUpgrader interface {
	piruntime.CurrentRuntimeReader
	ReadStart(context.Context, agentbackend.SessionRef) (piruntime.StartRequest, error)
	Upgrade(context.Context, piruntime.UpgradeRequest, piruntime.UpgradeHooks) (piruntime.UpgradeResult, error)
}

// This ordinary operator entry supplies only an operation identity and the
// predecessor expectation. Package, profile and bound members stay server-owned.
func (s *Server) piRuntimeUpgradeAPI(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(r.Header.Values("Origin")) != 1 || !slices.Contains(s.trustedOrigins, r.Header.Get("Origin")) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	var input struct {
		Workspace          string `json:"workspace"`
		RequestID          string `json:"requestId"`
		ExpectedGeneration string `json:"expectedGeneration"`
	}
	if !workflowBody(w, r, &input) {
		return
	}
	if input.Workspace != s.config.Workspace || len(input.RequestID) != 36 || !queueClientMessageIDPattern.MatchString(input.RequestID) || !messageDigestPattern.MatchString(input.ExpectedGeneration) || r.URL.RawQuery != "" {
		http.Error(w, "Invalid runtime upgrade request", http.StatusBadRequest)
		return
	}
	if s.config.PiCreation == nil || s.piWorkflows == nil {
		workflowError(w, piruntime.ErrUnavailable)
		return
	}
	runtime, ok := s.config.PiCreation.Runtime.(piRuntimeUpgrader)
	if !ok {
		workflowError(w, piruntime.ErrUnavailable)
		return
	}
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: input.Workspace}).Load(slug)
	if err != nil || binding.Backend != agentbackend.Pi {
		workflowError(w, agentbackend.ErrBindingConflict)
		return
	}
	ref := agentbackend.SessionRef{Workspace: binding.Workspace, Slug: binding.Slug, Incarnation: binding.SessionIncarnation}
	current, release, err := s.acquirePiSessionAdmissionFor(r.Context(), ref, true, true)
	if err != nil {
		workflowError(w, err)
		return
	}
	defer release()
	if err := validatePiPackage(s.config, s.hostProfile); err != nil {
		workflowError(w, err)
		return
	}
	previous, err := runtime.ReadCurrentRuntime(r.Context(), ref)
	if err != nil || previous.Session != ref {
		workflowError(w, errors.Join(piruntime.ErrHeld, err))
		return
	}
	start := piruntime.StartRequest{Session: ref, BindingRevision: current.Binding.Revision,
		RequestID: input.RequestID, ReplacesRequestID: previous.RequestID, AppStorePath: s.config.PackageRoot,
		ProfilePath: s.config.HostProfile, ProfileGeneration: piProfileToken(s.hostProfile)}
	if previous.RequestID == input.RequestID {
		// Recovery uses the frozen request, including its original predecessor.
		// A changed selected target/profile may not silently replace that intent.
		saved, err := runtime.ReadStart(r.Context(), ref)
		if err != nil || saved.Session != ref || saved.RequestID != input.RequestID || saved.ReplacesRequestID == "" || saved.BindingRevision != start.BindingRevision || saved.AppStorePath != start.AppStorePath || saved.ProfilePath != start.ProfilePath || saved.ProfileGeneration != start.ProfileGeneration {
			workflowError(w, errors.Join(piruntime.ErrConflict, err))
			return
		}
		start = saved
	} else if previous.Generation != input.ExpectedGeneration || current.Authority.Generation != input.ExpectedGeneration {
		workflowError(w, agentbackend.ErrStaleExecution)
		return
	}
	members := make([]agentbackend.ConversationRef, 0, len(current.Binding.Conversations))
	for _, member := range current.Binding.Conversations {
		bound, err := current.Binding.Resolve(member.Address)
		if err != nil {
			workflowError(w, err)
			return
		}
		members = append(members, bound)
	}
	result, err := runtime.Upgrade(r.Context(), piruntime.UpgradeRequest{Start: start, ExpectedGeneration: input.ExpectedGeneration, Members: members}, piruntime.UpgradeHooks{
		EnsureAttachment: s.piWorkflows.ensureAttachmentWithConnector,
		Publish: func(ctx context.Context, proof piruntime.UpgradeProof) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := validatePiPackage(s.config, s.hostProfile); err != nil {
				return err
			}
			latest, err := s.piReadySessionIdentityLocked(ref)
			if err != nil || !reflect.DeepEqual(latest.Binding, current.Binding) || !reflect.DeepEqual(latest.intent, current.intent) {
				return errors.Join(agentbackend.ErrBindingConflict, err)
			}
			_, err = session.PublishPiRuntimeUpgradeAuthority(s.config.AuthorityDir, latest.Binding, current.Authority, proof)
			if err == nil {
				for _, member := range members {
					s.piWorkflows.invalidateRef(member, false)
				}
			}
			return err
		},
	})
	if err != nil {
		workflowError(w, err)
		return
	}
	workflowJSON(w, http.StatusOK, result)
}
