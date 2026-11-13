package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// Only activity comes from the observer. Delivery and recovery remain with the
// operational client and its existing provider, workspace and receipt policy.
type communicationClient struct {
	communication.NativeClient
	activity *activityMonitor
}

func (c communicationClient) ReadActivity(ctx context.Context, thread string) (codex.ActivitySnapshot, error) {
	var snapshot codex.ActivitySnapshot
	var err error
	if c.activity != nil {
		snapshot, err = c.activity.readFreshActivity(ctx, thread)
	} else {
		snapshot, err = c.NativeClient.ReadActivity(ctx, thread)
	}
	if err != nil {
		return codex.ActivitySnapshot{}, err
	}
	if snapshot.Stale || snapshot.ThreadID != thread {
		return codex.ActivitySnapshot{}, errors.New("recipient activity is stale or belongs to another thread")
	}
	return snapshot, nil
}

func (s *Server) communicationService(invocation ...*piPackageInvocation) (communication.Service, error) {
	client, ok := s.config.Codex.(communication.NativeClient)
	if !ok || s.config.UserStateRoot == "" {
		return communication.Service{}, errors.New("session communication is unavailable")
	}
	service, err := (communication.Native{Workspace: s.config.Workspace, StateRoot: s.config.UserStateRoot, AuthorityDir: s.config.AuthorityDir, Socket: s.config.CodexSocket, Client: communicationClient{NativeClient: client, activity: s.activity}, Admission: func(id communication.Identity) error { return s.requireProviderAdmission(id.Slug) }}).Service()
	if err == nil && len(invocation) == 1 && invocation[0] != nil {
		if invocation[0].file != nil {
			// Establish storage compatibility before Service.Run/Dispatch take
			// sorted runtime locks, then recheck only at Schema2Write.
			if err := invocation[0].recheck(); err != nil {
				return communication.Service{}, err
			}
		}
		service.Store.Schema2Write = invocation[0].withWrite
	}
	return service, err
}

func (s *Server) communicationAPI(w http.ResponseWriter, r *http.Request, summary *session.Summary, invocation ...*piPackageInvocation) {
	service, err := s.communicationService(invocation...)
	if err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, err.Error())
		return
	}
	caller, err := communication.IdentityFor(s.config.Workspace, summary.Slug, r.Method == http.MethodGet)
	if err != nil {
		s.writeError(w, r, http.StatusConflict, err.Error())
		return
	}
	if r.Method == http.MethodGet {
		view, err := service.Read(r.Context(), caller)
		if err != nil {
			s.writeError(w, r, http.StatusServiceUnavailable, err.Error())
			return
		}
		candidates := []string{}
		all, err := session.List(s.config.Workspace)
		if err != nil {
			s.writeError(w, r, http.StatusServiceUnavailable, "Sessions could not be listed.")
			return
		}
		for _, other := range all {
			if other.Slug != caller.Slug && !other.Archived && other.Codex.ThreadID != "" && other.Creation.State != "creating" {
				candidates = append(candidates, other.Slug)
			}
		}
		type presentedMessage struct {
			communication.Message
			HTML string `json:"html"`
		}
		messages := make([]presentedMessage, 0, len(view.Messages))
		for _, message := range view.Messages {
			messages = append(messages, presentedMessage{Message: message, HTML: string(s.renderTextMarkdown(message.Text))})
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"grants": view.Grants, "messages": messages, "candidates": candidates})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		s.writeError(w, r, http.StatusMethodNotAllowed, "Unsupported communication operation.")
		return
	}
	var request communication.Request
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if receipt, ok := s.currentCreation(summary.Slug); ok && receipt.blocksSession() {
		s.writeError(w, r, http.StatusConflict, "Session initialization has not finished.")
		return
	}
	if request.Action == "allow" {
		request.Approval = "User approved this pair in the portal from " + summary.Slug
	}
	view, err := service.Run(r.Context(), caller, request)
	if err != nil {
		s.writeError(w, r, http.StatusConflict, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

func (s *Server) startCommunication() {
	service, err := s.communicationService()
	if err != nil {
		return
	}
	s.operationWG.Add(1)
	go func() {
		defer s.operationWG.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		next := 0
		var lastError string
		for {
			select {
			case <-s.operationContext.Done():
				return
			case <-ticker.C:
			}
			ctx, cancel := context.WithTimeout(s.operationContext, 20*time.Second)
			messages, err := service.Pending(ctx)
			if err == nil {
				for count := 0; count < len(messages) && count < 16; count++ {
					if ctx.Err() != nil {
						break
					}
					m := messages[next%len(messages)]
					next++
					dispatchErr := s.dispatchCommunication(ctx, m.ID)
					if dispatchErr != nil {
						err = dispatchErr
						break
					}
				}
			}
			if err != nil && err.Error() != lastError {
				s.config.Logger.Printf("session communication: %v", err)
				lastError = err.Error()
			} else if err == nil {
				lastError = ""
			}
			cancel()
		}
	}()
}

func (s *Server) dispatchCommunication(ctx context.Context, id string) error {
	return s.withCurrentGenerationInvocation(func(invocation *piPackageInvocation) error {
		admitted, err := s.communicationService(invocation)
		if err != nil {
			return err
		}
		return admitted.Dispatch(ctx, id)
	})
}
