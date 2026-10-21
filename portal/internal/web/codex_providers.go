package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type providerView struct {
	Providers []codexproviders.PublicProvider `json:"providers"`
	Active    string                          `json:"active"`
	Operation *publicProviderOperation        `json:"operation,omitempty"`
	Default   string                          `json:"default"`
	Session   string                          `json:"session,omitempty"`
	Inherited bool                            `json:"inherited"`
}

type publicProviderOperation struct {
	ID              string   `json:"id"`
	Target          string   `json:"target"`
	Phase           string   `json:"phase"`
	Blockers        []string `json:"blockers"`
	Message         string   `json:"message,omitempty"`
	Session         string   `json:"session,omitempty"`
	Stage           string   `json:"stage,omitempty"`
	Thread          string   `json:"thread,omitempty"`
	ThreadLabel     string   `json:"threadLabel,omitempty"`
	CanCancel       bool     `json:"canCancel"`
	CancelRequested bool     `json:"cancelRequested,omitempty"`
	Dismissed       bool     `json:"dismissed,omitempty"`
	FinishedAt      string   `json:"finishedAt,omitempty"`
	Completed       int      `json:"completed"`
	Total           int      `json:"total"`
	CreatedAt       string   `json:"createdAt,omitempty"`
	UpdatedAt       string   `json:"updatedAt,omitempty"`
	Reason          string   `json:"reason,omitempty"`
}

func (s *Server) providerState() (codexproviders.Catalog, codexproviders.State, error) {
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		return catalog, codexproviders.State{}, err
	}
	state, err := codexproviders.ReadState(s.config.UserStateRoot, s.config.WorkspaceName, s.config.Workspace, s.config.CodexSocket, catalog)
	return catalog, state, err
}

func (s *Server) providerView() (providerView, error) {
	return s.providerSessionView("")
}

func (s *Server) providerSessionView(slug string) (providerView, error) {
	catalog, state, err := s.providerState()
	if err != nil {
		return providerView{}, err
	}
	view := providerView{Providers: []codexproviders.PublicProvider{}, Active: state.Active, Default: state.Active, Session: slug, Inherited: true}
	if slug != "" {
		summary, err := session.Find(s.config.Workspace, slug)
		if err != nil {
			return providerView{}, err
		}
		view.Active = state.Provider(slug, summary.Codex.ThreadID)
		selection, selected := state.Sessions[slug]
		view.Inherited = !selected || selection.RootThreadID != summary.Codex.ThreadID
	}
	for _, provider := range catalog.Providers {
		view.Providers = append(view.Providers, provider.Public())
	}
	if o := state.Operation; o != nil {
		view.Operation = &publicProviderOperation{ID: o.ID, Target: o.Target, Phase: o.Phase, Blockers: o.Blockers, Message: o.Message, Session: o.Session, Stage: o.Stage, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt}
		view.Operation.CancelRequested, view.Operation.Dismissed = o.CancelRequested, o.Dismissed
		view.Operation.CanCancel = !o.CancelRequested && (o.Phase == "waiting" || o.Strategy == "scoped-v1" && o.Phase == "applying" && (o.Stage == "quiescing" || o.Stage == "preparing"))
		if o.Phase == "applied" || o.Phase == "failed" || o.Phase == "canceled" {
			view.Operation.FinishedAt = o.UpdatedAt
		}
		if o.Strategy == "scoped-v1" {
			path := filepath.Join(s.config.UserStateRoot, "providers", s.config.WorkspaceName, o.ID+".snapshot.json.progress")
			if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0600 && info.Size() <= 64*1024 {
				data, err := os.ReadFile(path)
				var progress struct {
					Operation   string `json:"operation"`
					Stage       string `json:"stage"`
					Thread      string `json:"thread"`
					ThreadLabel string `json:"threadLabel"`
					Completed   int    `json:"completed"`
					Total       int    `json:"total"`
					UpdatedAt   string `json:"updatedAt"`
					Reason      string `json:"reason"`
				}
				if err == nil && json.Unmarshal(data, &progress) == nil && progress.Operation == o.ID {
					view.Operation.Stage, view.Operation.Thread = progress.Stage, progress.Thread
					view.Operation.ThreadLabel = progress.ThreadLabel
					view.Operation.Completed, view.Operation.Total = progress.Completed, progress.Total
					view.Operation.UpdatedAt, view.Operation.Reason = progress.UpdatedAt, progress.Reason
				}
			}
		}
	}
	return view, nil
}

func (s *Server) codexProviders(w http.ResponseWriter, r *http.Request) {
	slug := r.URL.Query().Get("session")
	if slug != "" && !session.ValidSlug(slug) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid provider session"})
		return
	}
	view, err := s.providerSessionView(slug)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Providers unavailable"})
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

func (s *Server) providerMutation(w http.ResponseWriter, r *http.Request, cancel bool) {
	recover := strings.HasSuffix(r.URL.Path, "/recover")
	dismiss := strings.HasSuffix(r.URL.Path, "/dismiss")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		ProviderID     string `json:"providerId"`
		IdempotencyKey string `json:"idempotencyKey"`
		OperationID    string `json:"operationId"`
		Session        string `json:"session"`
		Inherit        bool   `json:"inherit"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	if body.Session != "" {
		if !session.ValidSlug(body.Session) {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid provider session"})
			return
		}
		if summary, err := session.Find(s.config.Workspace, body.Session); err != nil || summary.Archived || summary.Codex.ThreadID == "" {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Provider session is unavailable"})
			return
		}
	}
	catalog, _, err := s.providerState()
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Providers unavailable"})
		return
	}
	action := "request"
	arguments := []string{body.ProviderID, body.IdempotencyKey}
	if cancel || recover || dismiss {
		action = "cancel"
		if recover {
			action = "recover"
		}
		if dismiss {
			action = "dismiss"
		}
		arguments = []string{body.OperationID}
		if !queueClientMessageIDPattern.MatchString(body.OperationID) {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid provider operation"})
			return
		}
	} else if _, err = catalog.Find(body.ProviderID); err != nil || !queueClientMessageIDPattern.MatchString(body.IdempotencyKey) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid provider request"})
		return
	}
	if !cancel && !recover && !dismiss && body.Session != "" {
		arguments = append(arguments, "--session", body.Session)
	}
	if body.Inherit {
		if cancel || recover || dismiss || body.Session == "" {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Select a session to use the workspace default"})
			return
		}
		arguments = append(arguments, "--inherit")
	}
	// The host command takes the generation/request locks itself. Holding a
	// second transition lock here would deadlock its exclusive apply admission.
	ctx, stop := context.WithTimeout(r.Context(), 15*time.Second)
	defer stop()
	command := exec.CommandContext(ctx, filepath.Join(s.config.HostProfile, "bin/workspace-host"), append([]string{"provider", action, s.config.WorkspaceName}, arguments...)...)
	diagnostic := boundedCoordinationBuffer{limit: 4096, truncate: true}
	command.Stderr = &diagnostic
	if command.Run() != nil {
		message := "Provider request could not be accepted. Refresh its status and retry."
		if strings.Contains(diagnostic.String(), "workspace package transition is in progress") {
			message = "A workspace package update is in progress. Retry after it finishes."
		}
		if strings.Contains(diagnostic.String(), "another provider request is pending") {
			message = "A provider switch is already running. Its progress is shown below."
		}
		if strings.Contains(diagnostic.String(), "provider preparation has ended") {
			message = "Preparation has ended. Check the switch result or use its recovery action."
		}
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": message})
		return
	}
	view, err := s.providerSessionView(body.Session)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Provider status unavailable"})
		return
	}
	s.writeJSON(w, http.StatusAccepted, view)
}

func (s *Server) requireProviderAdmission(slugs ...string) error {
	_, state, err := s.providerState()
	if errors.Is(err, os.ErrNotExist) {
		// A removed catalog must not hide an existing reservation.
		path, pathErr := codexproviders.StatePath(s.config.UserStateRoot, s.config.WorkspaceName)
		if pathErr != nil {
			if s.config.WorkspaceName == "" {
				return nil
			}
			return pathErr
		}
		if _, stateErr := os.Lstat(path); errors.Is(stateErr, os.ErrNotExist) {
			return nil
		}
	}
	if err != nil {
		return err
	}
	if state.Operation.Reserved() {
		if state.Operation.Strategy == "scoped-v1" {
			if len(slugs) == 0 || slugs[0] == "" {
				return nil
			}
			summary, err := session.Find(s.config.Workspace, slugs[0])
			if err != nil {
				return err
			}
			if !state.BlocksSession(slugs[0], summary.Codex.ThreadID) {
				return nil
			}
		}
		return errors.New("workspace provider transition is reserved; wait for completion or recover it")
	}
	return nil
}

func (s *Server) validateCreationProvider(id string) error {
	if id == "" {
		return nil
	}
	catalog, _, err := s.providerState()
	if err != nil {
		return errors.New("provider catalog is unavailable")
	}
	_, err = catalog.Find(id)
	return err
}

func (s *Server) loadProviderLimits(ctx context.Context, id string) (codexLimitsSnapshot, error) {
	catalog, state, err := s.providerState()
	if errors.Is(err, os.ErrNotExist) && id == "" {
		if err := s.requireProviderAdmission(); err != nil {
			return codexLimitsSnapshot{}, err
		}
		return s.loadCodexLimits(ctx)
	}
	if err != nil {
		return codexLimitsSnapshot{}, err
	}
	if id == "" {
		id = state.Active
	}
	p, err := catalog.Find(id)
	if err != nil {
		return codexLimitsSnapshot{}, err
	}
	if p.Kind == "subscription" {
		result, err := s.loadCodexLimits(ctx)
		result.Provider = p.Public()
		result.CanReset = result.CanReset && state.Active == p.ID && !state.Operation.Reserved()
		return result, err
	}
	identity, err := codexproviders.CredentialIdentity(p)
	if err != nil {
		return codexLimitsSnapshot{}, err
	}
	key := catalog.Digest + "/" + id + "/" + identity
	s.codexLimitsMu.Lock()
	if s.providerLimits == nil {
		s.providerLimits = map[string]*codexLimitsCall{}
	}
	if call := s.providerLimits[key]; call != nil {
		complete := false
		select {
		case <-call.done:
			complete = true
		default:
		}
		if !complete || time.Since(call.result.UpdatedAt) < 30*time.Second {
			s.codexLimitsMu.Unlock()
			defer readStage(ctx, "limits_cache_wait")()
			select {
			case <-call.done:
				return call.result, call.err
			case <-ctx.Done():
				return codexLimitsSnapshot{}, ctx.Err()
			}
		}
	}
	if len(s.providerLimits) > 128 {
		s.providerLimits = map[string]*codexLimitsCall{}
	}
	call := &codexLimitsCall{done: make(chan struct{})}
	s.providerLimits[key] = call
	s.codexLimitsMu.Unlock()
	readCtx, stop := context.WithTimeout(s.operationContext, 10*time.Second)
	defer stop()
	metrics, err := codexproviders.ReadMetrics(readCtx, p)
	call.err = err
	if err == nil {
		call.result = codexLimitsSnapshot{Windows: []codex.RateLimitWindow{}, Provider: p.Public(), Metrics: &metrics, UpdatedAt: time.Now().UTC()}
		for _, entry := range []struct {
			key      string
			duration int64
		}{{"primary", 300}, {"secondary", 10080}} {
			if remaining, ok := metrics.PoolRemaining[entry.key]; ok {
				duration := entry.duration
				call.result.Windows = append(call.result.Windows, codex.RateLimitWindow{UsedPercent: int(100 - remaining), WindowDurationMins: &duration})
			}
		}
	}
	s.codexLimitsMu.Lock()
	if err != nil {
		delete(s.providerLimits, key)
	}
	close(call.done)
	s.codexLimitsMu.Unlock()
	return call.result, call.err
}
