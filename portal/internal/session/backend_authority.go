package session

import (
	"errors"
	"fmt"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// PiRuntimeAuthority is a live projection of a durable binding. It proves no
// process/socket/namespace ownership and cannot independently select a backend.
type PiRuntimeAuthority struct {
	Schema             int               `json:"schema"`
	Backend            agentbackend.Kind `json:"backend"`
	BackendContract    int               `json:"backend_contract"`
	Workspace          string            `json:"workspace"`
	Slug               string            `json:"slug"`
	SessionIncarnation string            `json:"session_incarnation"`
	BindingRevision    uint64            `json:"binding_revision"`
	Generation         string            `json:"generation"`
	State              string            `json:"state"`
}

func LoadPiRuntimeAuthority(directory, slug, workspace string) (PiRuntimeAuthority, error) {
	if !backendCanonicalPath(directory) || !backendCanonicalPath(workspace) || !backendSlug(slug) {
		return PiRuntimeAuthority{}, errors.New("invalid Pi runtime authority scope")
	}
	data, err := readBackendRecord(directory, slug+".json")
	if err != nil {
		return PiRuntimeAuthority{}, err
	}
	var authority PiRuntimeAuthority
	if err := decodeBackendRecord(data, &authority, []string{
		"schema", "backend", "backend_contract", "workspace", "slug", "session_incarnation",
		"binding_revision", "generation", "state",
	}); err != nil {
		return PiRuntimeAuthority{}, fmt.Errorf("decode Pi runtime authority: %w", err)
	}
	if err := authority.validate(workspace, slug); err != nil {
		return PiRuntimeAuthority{}, err
	}
	return authority, nil
}

func (a PiRuntimeAuthority) validate(workspace, slug string) error {
	if a.Schema != 2 || a.Backend != agentbackend.Pi || a.BackendContract != 1 {
		return agentbackend.ErrUnsupportedBinding
	}
	if !backendCanonicalPath(workspace) || !backendSlug(slug) || a.Workspace != workspace || a.Slug != slug ||
		!isLowerHex(a.SessionIncarnation, 64) || a.BindingRevision == 0 || !isLowerHex(a.Generation, 64) ||
		(a.State != "starting" && a.State != "ready") {
		return errors.New("invalid Pi runtime authority identity")
	}
	return nil
}

// ValidateBinding compares only durable identity/revision, never generation.
// A matching or ready projection is not permission to dispatch native work.
func (a PiRuntimeAuthority) ValidateBinding(b BackendBinding) error {
	if err := b.Validate(b.Workspace, b.Slug); err != nil {
		return err
	}
	if err := a.validate(a.Workspace, a.Slug); err != nil {
		return err
	}
	if a.Workspace != b.Workspace || a.Slug != b.Slug || a.SessionIncarnation != b.SessionIncarnation || a.BindingRevision != b.Revision {
		return agentbackend.ErrBindingConflict
	}
	return nil
}

// ValidateHandle rejects stale worker/member handles. The caller must still
// verify current roster authorization, admission and native runtime ownership.
func (a PiRuntimeAuthority) ValidateHandle(b BackendBinding, handle agentbackend.ExecutionHandle) error {
	if err := a.ValidateBinding(b); err != nil {
		return err
	}
	current, err := b.Resolve(handle.Conversation.Member)
	if err != nil {
		return err
	}
	if a.State != "ready" {
		return agentbackend.ErrConversationNotReady
	}
	if current != handle.Conversation || a.Generation != handle.Generation {
		return agentbackend.ErrStaleExecution
	}
	return nil
}
