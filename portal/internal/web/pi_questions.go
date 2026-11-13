package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
)

// PiWorkflowAdmission is supplied by the existing conversation route owner. It
// resolves opaque browser identity, permissions, shutdown and current scope. It
// must never start/load a native worker to authorize a read.
type PiWorkflowAdmission func(context.Context, *http.Request, string) (agentbackend.ConversationRef, error)
type PiQuestionHTTPStore interface {
	piquestions.Store
	Response(context.Context, agentbackend.ConversationRef, string) (piquestions.ResponseView, error)
}
type PiQuestionHandler struct {
	Origin  string
	Resolve PiWorkflowAdmission
	Store   PiQuestionHTTPStore
}

func workflowError(w http.ResponseWriter, err error) {
	code := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, piquestions.ErrInvalid):
		code = 400
	case errors.Is(err, piquestions.ErrCapacity):
		code = 413
	case errors.Is(err, piquestions.ErrConflict), errors.Is(err, agentbackend.ErrStaleExecution), errors.Is(err, agentbackend.ErrBindingConflict):
		code = 409
	case errors.Is(err, piquestions.ErrNotFound):
		code = 404
	case errors.Is(err, piquestions.ErrUnavailable):
		code = 501
	}
	http.Error(w, http.StatusText(code), code)
}
func workflowRequest(w http.ResponseWriter, r *http.Request, origin string, resolve PiWorkflowAdmission, op string) (context.Context, context.CancelFunc, agentbackend.ConversationRef, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	if origin == "" || resolve == nil {
		cancel()
		workflowError(w, piquestions.ErrUnavailable)
		return nil, nil, agentbackend.ConversationRef{}, false
	}
	if values, got := r.Header.Values("Origin"), r.Header.Get("Origin"); len(values) > 1 || got != origin && (r.Method != "GET" || got != "") {
		cancel()
		http.Error(w, "Forbidden", 403)
		return nil, nil, agentbackend.ConversationRef{}, false
	}
	ref, err := resolve(ctx, r, op)
	if err != nil {
		cancel()
		http.Error(w, "Forbidden", 403)
		return nil, nil, ref, false
	}
	if piquestions.ValidateRef(ref) != nil {
		cancel()
		workflowError(w, piquestions.ErrUnavailable)
		return nil, nil, ref, false
	}
	return ctx, cancel, ref, true
}
func workflowBody(w http.ResponseWriter, r *http.Request, out any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if len(r.Header.Values("Content-Type")) != 1 {
		workflowError(w, piquestions.ErrInvalid)
		return false
	}
	if err != nil || media != "application/json" {
		workflowError(w, piquestions.ErrInvalid)
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		workflowError(w, piquestions.ErrCapacity)
		return false
	}
	if err = piquestions.Decode(data, out); err != nil {
		workflowError(w, piquestions.ErrInvalid)
		return false
	}
	return true
}
func workflowJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (h PiQuestionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		workflowError(w, piquestions.ErrUnavailable)
		return
	}
	path := strings.Trim(r.URL.Path, "/")
	op := "pi-questions.read"
	if r.Method == "POST" {
		op = "pi-questions.respond"
	}
	ctx, cancel, ref, ok := workflowRequest(w, r, h.Origin, h.Resolve, op)
	if !ok {
		return
	}
	defer cancel()
	switch {
	case r.Method == "GET" && strings.HasSuffix(path, "pi-questions"):
		v, err := h.Store.Snapshot(ctx, ref)
		if err != nil {
			workflowError(w, err)
			return
		}
		workflowJSON(w, 200, v)
	case r.Method == "POST" && strings.HasSuffix(path, "pi-questions/responses"):
		var input piquestions.ResponseInput
		if !workflowBody(w, r, &input) {
			return
		}
		v, err := h.Store.AcceptResponse(ctx, ref, input)
		if err != nil {
			workflowError(w, err)
			return
		}
		workflowJSON(w, 202, v)
	case r.Method == "GET" && strings.Contains(path, "pi-questions/responses/"):
		id := path[strings.LastIndex(path, "/")+1:]
		if !piquestions.ValidID(id) {
			workflowError(w, piquestions.ErrInvalid)
			return
		}
		v, err := h.Store.Response(ctx, ref, id)
		if err != nil {
			workflowError(w, err)
			return
		}
		workflowJSON(w, 200, v)
	default:
		http.Error(w, "Method Not Allowed", 405)
	}
}
