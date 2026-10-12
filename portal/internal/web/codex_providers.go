package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
)

type providerView struct {
	Providers []codexproviders.PublicProvider `json:"providers"`
	Active    string                          `json:"active"`
	Operation *publicProviderOperation        `json:"operation,omitempty"`
}

type publicProviderOperation struct {
	ID       string   `json:"id"`
	Target   string   `json:"target"`
	Phase    string   `json:"phase"`
	Blockers []string `json:"blockers"`
	Message  string   `json:"message,omitempty"`
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
	catalog, state, err := s.providerState()
	if err != nil {
		return providerView{}, err
	}
	view := providerView{Providers: []codexproviders.PublicProvider{}, Active: state.Active}
	for _, provider := range catalog.Providers {
		view.Providers = append(view.Providers, provider.Public())
	}
	if o := state.Operation; o != nil {
		view.Operation = &publicProviderOperation{ID: o.ID, Target: o.Target, Phase: o.Phase, Blockers: o.Blockers, Message: o.Message}
	}
	return view, nil
}

func (s *Server) codexProviders(w http.ResponseWriter, r *http.Request) {
	view, err := s.providerView()
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Providers unavailable"})
		return
	}
	s.writeJSON(w, http.StatusOK, view)
}

func (s *Server) providerMutation(w http.ResponseWriter, r *http.Request, cancel bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		ProviderID     string `json:"providerId"`
		IdempotencyKey string `json:"idempotencyKey"`
		OperationID    string `json:"operationId"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	catalog, _, err := s.providerState()
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Providers unavailable"})
		return
	}
	action := "request"
	arguments := []string{body.ProviderID, body.IdempotencyKey}
	if cancel {
		action = "cancel"
		arguments = []string{body.OperationID}
		if !queueClientMessageIDPattern.MatchString(body.OperationID) {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid provider operation"})
			return
		}
	} else if _, err = catalog.Find(body.ProviderID); err != nil || !queueClientMessageIDPattern.MatchString(body.IdempotencyKey) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid provider request"})
		return
	}
	// The host command takes the generation/request locks itself. Holding a
	// second transition lock here would deadlock its exclusive apply admission.
	ctx, stop := context.WithTimeout(r.Context(), 15*time.Second)
	defer stop()
	command := exec.CommandContext(ctx, filepath.Join(s.config.HostProfile, "bin/workspace-host"), append([]string{"provider", action, s.config.WorkspaceName}, arguments...)...)
	if command.Run() != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Provider request could not be accepted. Refresh its status and retry."})
		return
	}
	view, err := s.providerView()
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Provider status unavailable"})
		return
	}
	s.writeJSON(w, http.StatusAccepted, view)
}

func (s *Server) requireProviderAdmission() error {
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
		return errors.New("workspace provider transition is reserved; wait for completion or recover it")
	}
	return nil
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
