package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
)

type PiPlanHTTPStore interface {
	Snapshot(context.Context, agentbackend.ConversationRef) (piplan.Snapshot, error)
	Accept(context.Context, agentbackend.ConversationRef, piplan.AcceptanceInput) (piplan.AcceptanceView, error)
	LookupAcceptance(context.Context, agentbackend.ConversationRef, string) (piplan.AcceptanceView, error)
}

// PiPlanHandler is deliberately unmounted. Store/policy availability is not a
// claim that the native adapter can enforce Plan or implement accepted bytes.
type PiPlanHandler struct {
	Origin  string
	Resolve PiWorkflowAdmission
	Store   PiPlanHTTPStore
}

func (h PiPlanHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		workflowError(w, piquestions.ErrUnavailable)
		return
	}
	op := "pi-plan.read"
	if r.Method == "POST" {
		op = "pi-plan.accept"
	}
	ctx, cancel, ref, ok := workflowRequest(w, r, h.Origin, h.Resolve, op)
	if !ok {
		return
	}
	defer cancel()
	path := strings.Trim(r.URL.Path, "/")
	switch {
	case r.Method == "GET" && strings.HasSuffix(path, "pi-plan"):
		v, err := h.Store.Snapshot(ctx, ref)
		if err != nil {
			workflowError(w, err)
			return
		}
		workflowJSON(w, 200, v)
	case r.Method == "POST" && strings.HasSuffix(path, "pi-plan/acceptances"):
		var in piplan.AcceptanceInput
		if !workflowBody(w, r, &in) {
			return
		}
		v, err := h.Store.Accept(ctx, ref, in)
		if err != nil {
			workflowError(w, err)
			return
		}
		workflowJSON(w, 202, v)
	case r.Method == "GET" && strings.Contains(path, "pi-plan/acceptances/"):
		id := path[strings.LastIndex(path, "/")+1:]
		if !piquestions.ValidID(id) {
			workflowError(w, piquestions.ErrInvalid)
			return
		}
		v, err := h.Store.LookupAcceptance(ctx, ref, id)
		if err != nil {
			workflowError(w, err)
			return
		}
		workflowJSON(w, 200, v)
	default:
		http.Error(w, "Method Not Allowed", 405)
	}
}
