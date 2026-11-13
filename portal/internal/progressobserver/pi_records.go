package progressobserver

import (
	"context"
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

var piObserverID = regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

type PiPendingRecord struct {
	ID               string                                 `json:"id"`
	Address          string                                 `json:"address"`
	Model            string                                 `json:"model"`
	Effort           string                                 `json:"effort"`
	Message          string                                 `json:"message"`
	StartedAt        time.Time                              `json:"startedAt"`
	EvidenceComplete bool                                   `json:"evidenceComplete,omitempty"`
	Conversation     *monitoroperation.PiConversationRecord `json:"conversation,omitempty"`
	Input            *monitoroperation.PiInputRecord        `json:"input,omitempty"`
	Settings         monitoroperation.PiSettingsRecord      `json:"settings"`
	Outcome          string                                 `json:"outcome,omitempty"`
}
type PiState struct {
	Schema           int                              `json:"schema"`
	Paused           bool                             `json:"paused"`
	IntervalMinutes  int                              `json:"intervalMinutes,omitempty"`
	LastCheck        time.Time                        `json:"lastCheck"`
	Pending          *PiPendingRecord                 `json:"pending,omitempty"`
	Concern          *Concern                         `json:"concern,omitempty"`
	Notice           *Notice                          `json:"notice,omitempty"`
	Unavailable      string                           `json:"unavailable,omitempty"`
	PreviousEvidence string                           `json:"previousEvidence,omitempty"`
	Backend          string                           `json:"backend"`
	Session          monitoroperation.PiSessionRecord `json:"session"`
	LeadIncarnation  string                           `json:"leadIncarnation"`
}

func (p PiState) key() string { return "pi:" + p.Session.Incarnation + ":" + p.LeadIncarnation }
func (p PiState) Validate() error {
	if p.Schema != 2 || p.Backend != "pi" || p.Session.Validate() != nil || len(p.LeadIncarnation) != 64 || p.IntervalMinutes < 0 || p.IntervalMinutes > 1440 {
		return piruntime.ErrScope
	}
	leader := monitoroperation.PiConversationRecord{Session: p.Session, Backend: "pi", Member: "lead", Incarnation: p.LeadIncarnation, NativeID: "validation"}
	if leader.Validate() != nil {
		return piruntime.ErrScope
	}
	if q := p.Pending; q != nil {
		if !piObserverID.MatchString(q.ID) || q.Address == "" || len(q.Address) > 64 || q.Model == "" || q.Effort == "" || strings.TrimSpace(q.Message) == "" || len(q.Message) > 20000 || q.StartedAt.IsZero() || q.Settings.Validate() != nil || q.Settings.Model != q.Model {
			return piruntime.ErrScope
		}
		if q.Conversation != nil && (q.Conversation.Validate() != nil || q.Conversation.Session != p.Session || q.Conversation.Member != q.Address) {
			return piruntime.ErrScope
		}
		if q.Input != nil && (q.Input.Validate() != nil || q.Conversation == nil || q.Input.Conversation != *q.Conversation || q.Input.OperationID != q.ID) {
			return piruntime.ErrScope
		}
		if q.Outcome != "" && (q.Input == nil || (q.Outcome != "accepted" && q.Outcome != "not_submitted" && q.Outcome != "unknown")) {
			return piruntime.ErrScope
		}
	}
	return nil
}
func DecodePiState(data []byte) (PiState, error) {
	var p PiState
	err := monitoroperation.DecodePiRecord(data, &p, 65536)
	if err == nil {
		err = p.Validate()
	}
	return p, err
}
func (s Store) LoadPi(scope monitoroperation.PiSessionRecord, lead string) (PiState, error) {
	p := PiState{Schema: 2, Backend: "pi", Session: scope, LeadIncarnation: lead}
	err := monitoroperation.ReadPiRecord(s.path(p.key()), &p, 65536)
	if err != nil {
		return PiState{}, err
	}
	if p.Session != scope || p.LeadIncarnation != lead {
		return PiState{}, piruntime.ErrScope
	}
	return p, p.Validate()
}
func (s Store) SavePi(ctx context.Context, admission monitoroperation.PiTeamWriteAdmission, generation string, next PiState) error {
	if next.Validate() != nil {
		return piruntime.ErrScope
	}
	return monitoroperation.WithPiRecordWrite(ctx, admission, next.Session, generation, func() error {
		if err := os.MkdirAll(s.Directory, 0700); err != nil {
			return err
		}
		lock, err := monitoroperation.LockPiRecord(ctx, s.path(next.key())+".lock")
		if err != nil {
			return err
		}
		defer lock.Close()
		old, err := s.LoadPi(next.Session, next.LeadIncarnation)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && old.Pending != nil && next.Pending != nil {
			a, b := *old.Pending, *next.Pending
			a.EvidenceComplete, b.EvidenceComplete = false, false
			a.Conversation, b.Conversation = nil, nil
			a.Input, b.Input = nil, nil
			a.Outcome, b.Outcome = "", ""
			if !reflect.DeepEqual(a, b) || old.Pending.Input != nil && !reflect.DeepEqual(old.Pending.Input, next.Pending.Input) || old.Pending.Conversation != nil && !reflect.DeepEqual(old.Pending.Conversation, next.Pending.Conversation) || old.Pending.Outcome != "" && old.Pending.Outcome != next.Pending.Outcome {
				return piruntime.ErrConflict
			}
		}
		if err == nil && old.Pending != nil && next.Pending == nil && (old.Pending.Outcome == "" || old.Pending.Outcome == "unknown" || old.Pending.Outcome == "not_submitted") {
			return piruntime.ErrHeld
		}
		return monitoroperation.WritePiRecord(s.path(next.key()), next, 65536)
	})
}
