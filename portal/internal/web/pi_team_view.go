package web

import (
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

// PiDerivedSolo is a view of original creation evidence, never a roster record.
// It deliberately carries no schema, revision, timestamps or new authority.
type PiDerivedSolo struct {
	LeadModel  string                       `json:"leadModel"`
	LeadEffort string                       `json:"leadEffort"`
	Members    []teamruntime.PiRosterMember `json:"members"`
}

func derivedPiSolo(current piSessionAdmission) (*PiDerivedSolo, error) {
	i, b := current.intent, current.Binding
	if i.Schema != 4 || i.DirectTeam.ID != "solo" || len(i.DirectTeam.Members) != 0 || len(i.Conversations) != 1 || len(b.Conversations) != 1 || b.State != "ready" || i.ValidateBinding(b) != nil || current.Authority.ValidateBinding(b) != nil || b.Conversations[0].Address != "lead" || b.Conversations[0].State != "ready" || b.Conversations[0].Incarnation != i.Conversations[0].Incarnation {
		return nil, agentbackend.ErrBindingConflict
	}
	return &PiDerivedSolo{LeadModel: i.DirectTeam.LeadModel, LeadEffort: i.DirectTeam.LeadEffort, Members: []teamruntime.PiRosterMember{}}, nil
}

func (d pageData) PiTeamList() *PiDerivedSolo {
	if d.PiTeam != nil {
		return &PiDerivedSolo{d.PiTeam.LeadModel, d.PiTeam.LeadEffort, d.PiTeam.Members}
	}
	return d.PiDerivedSolo
}
