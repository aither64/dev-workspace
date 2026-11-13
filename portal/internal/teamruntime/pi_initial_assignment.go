package teamruntime

import (
	"context"
	"reflect"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// PiInitialAssignment reserves only the first REAL assignment reference in the
// existing roster. Native initialization/Send remain with the admitted runtime
// owner; there is no synthetic materialization prompt or hidden replacement ID.
type PiInitialAssignment struct {
	Session                         agentbackend.SessionRef
	Member, Incarnation, CreationID string
	Request                         backend.SendRequest
	Settings                        piruntime.EffectiveSettings
}

func (s *Store) ReservePiInitialAssignment(ctx context.Context, admission monitoroperation.PiTeamWriteAdmission, caller PiCaller, expectedRosterRevision uint64, message PiMessage, settings piruntime.EffectiveSettings, ready ...PiRosterReadyVerifier) (PiInitialAssignment, error) {
	if ValidatePiCaller(caller) != nil || ValidatePiMessage(message) != nil || message.To == "lead" || !validPiSettings(settings) {
		return PiInitialAssignment{}, ErrPiPolicy
	}
	current, err := s.LoadPi(caller.Runtime.Session.Slug)
	if err != nil {
		return PiInitialAssignment{}, err
	}
	if current.Scope().Ref() != caller.Runtime.Session {
		return PiInitialAssignment{}, ErrPiConflict
	}
	var selected *PiRosterMember
	for i := range current.Members {
		if current.Members[i].Address == message.To {
			selected = &current.Members[i]
			break
		}
	}
	if selected == nil || selected.State != "creating" || selected.Model != settings.Model {
		return PiInitialAssignment{}, ErrPiHeld
	}
	if selected.InitialInputID != "" && selected.InitialInputID != message.ID {
		return PiInitialAssignment{}, ErrPiConflict
	}
	if selected.InitialInputID == message.ID {
		// Replay retains the same reservation without consuming another roster
		// revision. P3 still checks its full immutable input/settings digest.
		err := monitoroperation.WithPiRecordWrite(ctx, admission, current.Scope(), caller.Runtime.Generation, func() error {
			latest, err := s.LoadPi(current.Slug)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(latest, current) {
				return ErrPiConflict
			}
			return nil
		})
		if err != nil {
			return PiInitialAssignment{}, err
		}
	} else {
		if current.Revision != expectedRosterRevision {
			return PiInitialAssignment{}, ErrPiConflict
		}
		selected.InitialInputID = message.ID
		current.Revision++
		current.UpdatedAt = time.Now().UTC()
		if err := s.SavePi(ctx, admission, caller.Runtime.Generation, expectedRosterRevision, current, ready...); err != nil {
			return PiInitialAssignment{}, err
		}
	}
	return PiInitialAssignment{Session: caller.Runtime.Session, Member: selected.Address, Incarnation: selected.Incarnation, CreationID: selected.CreationID, Settings: settings, Request: backend.SendRequest{ClientUserMessageID: message.ID, Message: PiEnvelope(caller, message), ActionContext: PiActionContext(caller, message)}}, nil
}

// Publish delegates only references to the EXISTING P2 PublishMemberReady port.
// Invoke after initialization/delivery ownership has been fully released. P2
// independently verifies persisted history/native witness and owns its locks.
func (a PiInitialAssignment) Publish(ctx context.Context, expectedBindingRevision uint64, handle agentbackend.ExecutionHandle, publish func(context.Context, uint64, agentbackend.ExecutionHandle, string, string) error) error {
	if publish == nil {
		return ErrPiUnavailable
	}
	if !validPiHandle(handle) || handle.Conversation.Session != a.Session || handle.Conversation.Member != a.Member || handle.Conversation.Incarnation != a.Incarnation || expectedBindingRevision == 0 || !digestPattern.MatchString(a.CreationID) || !messageIDPattern.MatchString(a.Request.ClientUserMessageID) {
		return ErrPiPolicy
	}
	return publish(ctx, expectedBindingRevision, handle, a.CreationID, a.Request.ClientUserMessageID)
}
