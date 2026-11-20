package web

import "github.com/aither64/dev-workspace/portal/internal/teamruntime"

type teamMemberView struct {
	Address, State, StateLabel, Thread, URL, Model, Effort, Access, Scope, Generation, SettingsReason, RemoveReason string
	Stats, CanSettings, CanRemove, CanRetry                                                                         bool
}
type teamManagementView struct {
	Rows, Removed             []teamMemberView
	CanAdd, CanCreate         bool
	Description, SettingsHelp string
}

func (d pageData) TeamManagement() teamManagementView {
	if d.TeamView != nil {
		return *d.TeamView
	}
	v := teamManagementView{CanAdd: d.Session != nil && d.Session.Interactive, CanCreate: d.Session != nil && d.Session.Interactive && d.DirectTeam == nil, Description: "The lead uses this session's main conversation. Each other member has a separate conversation.", SettingsHelp: "Changes apply to the next turn. Stopped sessions keep the choice until activation."}
	if list := d.PiTeamList(); list != nil {
		v.CanCreate = false
		v.CanAdd = d.ConversationInteractive
		v.SettingsHelp = "Save changes when the session is idle."
		v.Rows = append(v.Rows, teamMemberView{Address: "lead", State: "ready", StateLabel: "root", Stats: true, Model: list.LeadModel, Effort: list.LeadEffort})
		for _, m := range list.Members {
			row := teamMemberView{Address: m.Address, State: m.State, StateLabel: m.State, Model: m.Model, Effort: m.Effort, Access: m.Access, SettingsReason: "This member is not ready.", Stats: m.State == "ready"}
			if m.State == "creating" && m.InitialInputID == "" {
				row.StateLabel = "Awaiting first assignment"
			}
			if m.State == "removed" {
				v.Removed = append(v.Removed, row)
			} else {
				v.Rows = append(v.Rows, row)
			}
		}
		return v
	}
	if d.Session == nil {
		return v
	}
	v.Rows = append(v.Rows, teamMemberView{Address: "lead", State: "ready", StateLabel: "root", Thread: d.Session.Codex.ThreadID, Stats: true, CanSettings: !d.Session.Archived})
	if d.DirectTeam != nil {
		for _, m := range d.DirectTeam.Members {
			if m.State != "removed" {
				v.Rows = append(v.Rows, codexTeamMemberView(m))
			}
		}
	}
	for _, m := range d.RemovedMembers {
		v.Removed = append(v.Removed, codexTeamMemberView(m))
	}
	return v
}
func codexTeamMemberView(m teamruntime.Member) teamMemberView {
	return teamMemberView{Address: m.Address, State: m.State, StateLabel: m.State, Thread: m.Thread, Model: m.Model, Effort: m.Effort, Access: m.Access, Stats: true, CanSettings: m.State == "ready", CanRemove: m.State == "ready", CanRetry: m.State == "creating"}
}
