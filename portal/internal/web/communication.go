package web

import (
	"context"
	"errors"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"net/http"
	"strings"
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
	if s.config.UserStateRoot == "" || !ok && (s.config.PiCreation == nil || s.config.PiCreation.Runtime == nil) {
		return communication.Service{}, errors.New("session communication is unavailable")
	}
	service, err := (communication.Native{Workspace: s.config.Workspace, StateRoot: s.config.UserStateRoot, AuthorityDir: s.config.AuthorityDir, Socket: s.config.CodexSocket, Client: communicationClient{NativeClient: client, activity: s.activity}, Admission: func(id communication.Identity) error { return s.requireProviderAdmission(id.Slug) }}).Service()
	if err == nil {
		if !ok {
			service.Transport = nil
		}
		service.ResolveEndpoint = func(slug string) (communication.EndpointRecord, error) {
			return communication.EndpointFor(s.config.Workspace, s.config.UserStateRoot, slug, false)
		}
		pi := communicationPiTransport{s}
		service.Pi = pi
		service.AuthorizeEndpoint = func(ctx context.Context, e communication.EndpointRecord) error {
			if e.Pi == nil {
				return nil
			}
			_, err := pi.current(ctx, e)
			return err
		}
	}
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
	caller, err := communication.EndpointFor(s.config.Workspace, s.config.UserStateRoot, summary.Slug, r.Method == http.MethodGet)
	if err != nil {
		s.writeError(w, r, http.StatusConflict, err.Error())
		return
	}
	if r.Method == http.MethodGet {
		if err := s.checkCommunicationSource(r, caller, false); err != nil {
			s.writeError(w, r, http.StatusConflict, err.Error())
			return
		}
		view, err := service.ReadEndpoint(r.Context(), caller)
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
			if other.Slug != caller.Slug() && !other.Archived && other.Creation.State != "creating" {
				if _, err := communication.EndpointFor(s.config.Workspace, s.config.UserStateRoot, other.Slug, false); err == nil {
					candidates = append(candidates, other.Slug)
				}
			}
		}
		type presentedMessage struct {
			communication.PiMessageRecord
			HTML string `json:"html"`
		}
		messages := make([]presentedMessage, 0, len(view.Messages))
		for _, message := range view.Messages {
			messages = append(messages, presentedMessage{PiMessageRecord: message, HTML: string(s.renderTextMarkdown(message.Text))})
		}
		generation := ""
		if caller.Pi != nil {
			authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, caller.Slug(), s.config.Workspace)
			if err != nil {
				s.writeError(w, r, http.StatusConflict, err.Error())
				return
			}
			generation = authority.Generation
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"grants": view.Grants, "messages": messages, "candidates": candidates, "generation": generation})
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
	if request.Action == "allow" && strings.TrimSpace(request.Approval) == "" {
		request.Approval = "User approved this pair in the portal from " + summary.Slug
	}
	// Repeat exact source headers after sorted pair-lock acquisition.
	originalAuthorize := service.AuthorizeEndpoint
	service.AuthorizeEndpoint = func(ctx context.Context, e communication.EndpointRecord) error {
		if e.Slug() == caller.Slug() {
			if err := s.checkCommunicationSource(r, e, true); err != nil {
				return err
			}
		}
		if originalAuthorize != nil {
			return originalAuthorize(ctx, e)
		}
		return nil
	}
	if err := s.checkCommunicationSource(r, caller, true); err != nil {
		s.writeError(w, r, http.StatusConflict, err.Error())
		return
	}
	view, err := service.RunEndpoint(r.Context(), caller, request)
	if err != nil {
		s.writeError(w, r, http.StatusConflict, err.Error())
		return
	}
	if request.Action == "list" {
		view.Messages = nil
	}
	if request.MessageID != "" {
		messages := view.Messages[:0]
		for _, message := range view.Messages {
			if message.ID == request.MessageID {
				messages = append(messages, message)
			}
		}
		view.Messages = messages
	}
	if len(view.Messages) > 10 {
		view.Messages = view.Messages[len(view.Messages)-10:]
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
			err := runCommunicationPass(ctx, service, &next, s.dispatchCommunication)
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

func runCommunicationPass(ctx context.Context, service communication.Service, next *int, dispatch func(context.Context, string) error) error {
	messages, err := service.PendingEndpoints(ctx)
	if err != nil {
		return err
	}
	for count := 0; count < len(messages) && count < 16; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		m := messages[*next%len(messages)]
		*next++
		if err := dispatch(ctx, m.ID); err != nil {
			return err
		}
	}
	return nil
}

// The header proves the selected CLI/browser source scope, not native origin.
// Native MCP root proof remains with its caller and is retained by the Codex
// ObservationIdentity through any forwarding/pair-lock wait.
func (s *Server) checkCommunicationSource(r *http.Request, e communication.EndpointRecord, required bool) error {
	values := r.Header.Values("X-Workspace-Conversation-Scope")
	generations := r.Header.Values("X-Workspace-Pi-Generation")
	if e.Pi == nil {
		if len(generations) != 0 || len(values) > 1 || required && len(values) != 1 {
			return piruntime.ErrScope
		}
		if len(values) == 1 && (e.Codex == nil || values[0] != e.Codex.Incarnation) {
			return piruntime.ErrScope
		}
		return nil
	}
	if len(values) != 1 || values[0] != PiConversationScope(e.Conversation()) || len(generations) > 1 || required && len(generations) != 1 {
		return piruntime.ErrScope
	}
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(e.Slug())
	if err != nil {
		return err
	}
	authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, e.Slug(), s.config.Workspace)
	if err != nil || authority.State != "ready" || authority.ValidateBinding(binding) != nil {
		return piruntime.ErrScope
	}
	lead, err := binding.Resolve("lead")
	if err != nil || lead != e.Conversation() {
		return piruntime.ErrScope
	}
	if len(generations) == 1 && generations[0] != authority.Generation {
		return piruntime.ErrScope
	}
	return nil
}
