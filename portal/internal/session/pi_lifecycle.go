package session

import (
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// PiLifecycleCapture belongs to the existing archive/removal/revive journal.
// It retains no prompt, transcript, credential or substitute execution owner.
type PiLifecycleSelector struct {
	Name, Path, SHA256 string
}

type PiLifecycleCapture struct {
	Callbacks      []piruntime.LifecycleCallback `json:"callbacks,omitempty"`
	Selectors      []PiLifecycleSelector         `json:"selectors"`
	Kind           string                        `json:"kind"`
	Contract       int                           `json:"contract"`
	Binding        BackendBinding                `json:"binding"`
	Authority      PiRuntimeAuthority            `json:"authority"`
	Runtime        piruntime.RuntimeRef          `json:"runtime"`
	RosterRevision uint64                        `json:"roster_revision"`
	Members        []piruntime.LifecycleMember   `json:"members"`
	Terminal       *piruntime.HostObservation    `json:"terminal,omitempty"`
	Successor      *piruntime.StartRequest       `json:"successor,omitempty"`
}

func (p PiLifecycleCapture) Ref() agentbackend.SessionRef {
	return agentbackend.SessionRef{Workspace: p.Binding.Workspace, Slug: p.Binding.Slug, Incarnation: p.Binding.SessionIncarnation}
}
func (p PiLifecycleCapture) Conversations() []agentbackend.ConversationRef {
	refs := []agentbackend.ConversationRef{}
	for _, member := range p.Members {
		if member.Conversation != nil {
			refs = append(refs, *member.Conversation)
		}
	}
	return refs
}
func (p PiLifecycleCapture) ExecutionConversations() []agentbackend.ConversationRef {
	refs := []agentbackend.ConversationRef{}
	for _, member := range p.Members {
		if member.Conversation != nil && member.Generation == p.Runtime.Generation {
			refs = append(refs, *member.Conversation)
		}
	}
	return refs
}
func (p PiLifecycleCapture) Validate(workspace, slug string) error {
	if p.Kind != "pi" || p.Contract != 1 || p.Binding.Validate(workspace, slug) != nil || p.Authority.ValidateBinding(p.Binding) != nil || p.Authority.Generation != p.Runtime.Generation || p.Runtime.Session != p.Ref() || !isLowerHex(p.Runtime.Generation, 64) || !isLowerHex(p.Runtime.Digest, 64) || !piInitialInputID.MatchString(p.Runtime.RequestID) || !piInitialInputID.MatchString(p.Runtime.InstanceID) || len(p.Members) != len(p.Binding.Conversations) {
		return errors.New("invalid Pi lifecycle capture")
	}
	names := map[string]bool{}
	for _, selector := range p.Selectors {
		if selector.Name == "" || names[selector.Name] || !backendCanonicalPath(selector.Path) || !isLowerHex(selector.SHA256, 64) {
			return errors.New("invalid Pi lifecycle selector")
		}
		names[selector.Name] = true
	}
	for _, name := range []string{"binding", "creation", "companion"} {
		if !names[name] {
			return errors.New("Pi lifecycle selector capture is incomplete")
		}
	}
	if !names["completion"] && (p.Binding.State != "creating" || p.Binding.Conversations[0].State != "creating" || !names["receipt"] || p.Successor != nil) {
		return errors.New("Pi lifecycle completion capture is missing")
	}
	if !names["authority"] && p.Terminal == nil {
		return errors.New("Pi lifecycle authority capture is missing")
	}
	for _, callback := range p.Callbacks {
		found, recipient := false, false
		for _, member := range p.Members {
			if member.Conversation != nil && *member.Conversation == callback.Conversation {
				found = true
			}
			if member.Slot.Member == callback.To {
				recipient = true
			}
		}
		if !found || !recipient || callback.Input.Conversation != callback.Conversation || callback.Input.Action != "send" || !isLowerHex(callback.Input.Digest, 64) || !(piInitialInputID.MatchString(callback.MessageID) || isLowerHex(callback.MessageID, 32)) || callback.Origin.AnchorEntryID == "" || callback.Origin.NativeToolCallID == "" || len(callback.Origin.SelectionToken) != len("linear-v1:")+64 || callback.Origin.SelectionToken[:len("linear-v1:")] != "linear-v1:" || !isLowerHex(callback.Origin.SelectionToken[len("linear-v1:"):], 64) || !(piInitialInputID.MatchString(callback.Input.OperationID) || isLowerHex(callback.Input.OperationID, 32)) || (callback.Origin.Tool == "report_to_lead" && callback.To != "lead") || (callback.Origin.Tool != "team_message" && callback.Origin.Tool != "report_to_lead") || !isLowerHex(callback.ArgumentsSHA256, 64) {
			return errors.New("invalid Pi terminal callback scope")
		}
	}
	for i, member := range p.Members {
		bound := p.Binding.Conversations[i]
		slot := member.Slot
		if slot.Member != bound.Address || slot.Incarnation != bound.Incarnation || slot.CreationID == "" || slot.NativeID != bound.NativeID || slot.State != bound.State {
			return errors.New("Pi lifecycle slot changed")
		}
		if member.Conversation != nil {
			ref := *member.Conversation
			if ref.Session != p.Ref() || ref.Backend != agentbackend.Pi || ref.Member != slot.Member || ref.Incarnation != slot.Incarnation || bound.NativeID != "" && ref.NativeID != bound.NativeID || ref.NativeID == "" || member.Settings == nil || !isLowerHex(member.Generation, 64) || (member.Generation != p.Runtime.Generation && bound.State != "creating") {
				return errors.New("Pi lifecycle native identity changed")
			}
		} else if bound.NativeID != "" || member.Settings != nil || member.HistoryRef != "" || member.Generation != "" {
			return errors.New("Pi lifecycle missing native owner")
		}
	}
	if p.Terminal != nil {
		t := p.Terminal
		if t.InstanceID != p.Runtime.InstanceID || t.RequestID != p.Runtime.RequestID || t.Generation != p.Runtime.Generation || t.Digest != p.Runtime.Digest || t.Phase != "terminal" || t.Outcome != "stopped" || t.DrainProof == nil || t.DrainProof["empty"] != true {
			return errors.New("invalid Pi lifecycle terminal proof")
		}
	}
	if p.Successor != nil {
		n := p.Successor
		if n.Session != p.Ref() || n.ReplacesRequestID != p.Runtime.RequestID || n.RequestID == p.Runtime.RequestID || !piInitialInputID.MatchString(n.RequestID) || n.BindingRevision != p.Binding.Revision {
			return errors.New("invalid Pi lifecycle successor")
		}
	}
	return nil
}
func DecodePiLifecycleCapture(data []byte, workspace, slug string) (PiLifecycleCapture, error) {
	var p PiLifecycleCapture
	if err := DecodePiCreationJSON(data, &p, maxLifecycleJournalBytes); err != nil {
		return p, err
	}
	return p, p.Validate(workspace, slug)
}
func validatePiLifecycleField(raw json.RawMessage, workspace, slug string) error {
	_, err := DecodePiLifecycleCapture(raw, workspace, slug)
	return err
}

// RetirePiLifecycleAuthority removes only the captured live projection after its
// journal has durably saved terminal proof. Retained binding/native data stay.
func RetirePiLifecycleAuthority(directory string, p PiLifecycleCapture) error {
	if p.Validate(p.Binding.Workspace, p.Binding.Slug) != nil || p.Terminal == nil {
		return errors.New("Pi retirement proof is missing")
	}
	current, err := LoadPiRuntimeAuthority(directory, p.Binding.Slug, p.Binding.Workspace)
	if errors.Is(err, agentbackend.ErrNotBound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != p.Authority {
		return agentbackend.ErrBindingConflict
	}
	fd, err := openAuthorityDirectory(directory)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Unlinkat(fd, p.Binding.Slug+".json", 0); err != nil {
		return err
	}
	return unix.Fsync(fd)
}
