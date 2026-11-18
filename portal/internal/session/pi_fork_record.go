package session

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

const MaxPiForkRecordBytes = 1 << 20

// PiForkMember separates inherited applied native state from the new desired
// policy. Its source selection/catalog revision is provenance, never a receipt.
type PiForkMember struct {
	Address            string                          `json:"address"`
	SourceIncarnation  string                          `json:"source_incarnation"`
	CreationID         string                          `json:"creation_id"`
	NativeID           string                          `json:"native_id,omitempty"`
	State              string                          `json:"state"`
	AddedAt            string                          `json:"added_at"`
	RemovedAt          string                          `json:"removed_at,omitempty"`
	ReservedToolPolicy *piruntime.ToolPolicyTarget     `json:"reserved_tool_policy,omitempty"`
	Origin             *piruntime.ForkOrigin           `json:"origin,omitempty"`
	Selection          *piproviders.VersionedSelection `json:"source_selection,omitempty"`
	SelectionSource    string                          `json:"selection_source,omitempty"`
	CatalogDigest      string                          `json:"source_catalog_digest,omitempty"`
	PolicyStored       bool                            `json:"source_policy_stored"`
	PolicyRevision     uint64                          `json:"source_policy_revision,omitempty"`
}
type PiForkIntent struct {
	Source                agentbackend.SessionRef `json:"source"`
	SourceCreationID      string                  `json:"source_creation_id"`
	SourceBindingRevision uint64                  `json:"source_binding_revision"`
	SourceRosterRevision  uint64                  `json:"source_roster_revision"`
	SelectedMember        string                  `json:"selected_member"`
	LeadAccess            string                  `json:"lead_access"`
	RuntimeRequestID      string                  `json:"runtime_request_id"`
	Members               []PiForkMember          `json:"members"`
}

func (i PiCreationIntent) RuntimeRequestID() string {
	if i.Fork != nil {
		return i.Fork.RuntimeRequestID
	}
	return i.InitialInputID
}
func (i PiCreationIntent) ForkMember(address string) (PiForkMember, bool) {
	if i.Fork != nil {
		for _, m := range i.Fork.Members {
			if m.Address == address {
				return m, true
			}
		}
	}
	return PiForkMember{}, false
}
func (i PiCreationIntent) ActiveConversations() []PiReservedConversation {
	if i.Fork == nil {
		return i.Conversations
	}
	out := []PiReservedConversation{}
	for _, c := range i.Conversations {
		m, ok := i.ForkMember(c.Address)
		if ok && m.State != "removed" {
			out = append(out, c)
		}
	}
	return out
}
func (i PiCreationIntent) InitialMemberSettings(address string) (piruntime.EffectiveSettings, bool) {
	if m, ok := i.ForkMember(address); ok && m.Origin != nil {
		return m.Origin.Snapshot.Settings, true
	}
	for _, m := range i.ProviderSelection.Members {
		if m.Address == address {
			return m.Settings.Effective(), true
		}
	}
	return piruntime.EffectiveSettings{}, false
}
func (i PiCreationIntent) validateForkShape() error {
	f := i.Fork
	if i.Schema != 5 || f == nil || i.Backend != agentbackend.Pi || i.BackendContract != 1 || i.GoalSHA256 != "" || i.InitialInputID != "" || !backendCanonicalPath(i.Workspace) || !backendSlug(i.Slug) || !isLowerHex(i.ReceiptID, 64) || !isLowerHex(i.DeletionHistorySHA256, 64) || !isLowerHex(i.SessionIncarnation, 64) || !backendScalar(i.Model) || !backendScalar(i.Effort) || !backendScalar(i.Provider) || len(i.Conversations) < 1 || len(i.Conversations) > 65 || i.Conversations[0].Address != "lead" || i.Binding().Validate(i.Workspace, i.Slug) != nil {
		return errors.New("invalid Pi fork identity")
	}
	if f.Source == i.SessionRef() || !backendCanonicalPath(f.Source.Workspace) || f.Source.Workspace != i.Workspace || !backendSlug(f.Source.Slug) || !isLowerHex(f.Source.Incarnation, 64) || !isLowerHex(f.SourceCreationID, 64) || f.SourceBindingRevision == 0 || !backendMember(f.SelectedMember) || (f.LeadAccess != "read_only" && f.LeadAccess != "workspace_write") || !piInitialInputID.MatchString(f.RuntimeRequestID) || len(f.Members) != len(i.Conversations) {
		return errors.New("invalid Pi fork source")
	}
	team := i.DirectTeam
	if !backendScalar(team.ID) || !isLowerHex(team.CatalogDigest, 64) || !isLowerHex(team.TeamDigest, 64) || len(team.Members)+1 != len(i.Conversations) || len(team.Roles) != len(i.Conversations) || team.Roles[0] != "lead" || team.LeadModel != i.Model || team.LeadEffort != i.Effort {
		return errors.New("invalid Pi fork saved team")
	}
	seen := map[string]bool{}
	native := map[string]bool{}
	selected := false
	for n, c := range i.Conversations {
		m := f.Members[n]
		if m.Address != c.Address || !isLowerHex(m.SourceIncarnation, 64) || m.SourceIncarnation == c.Incarnation || !isLowerHex(m.CreationID, 64) || seen[c.Incarnation] || m.AddedAt == "" || (m.State != "ready" && m.State != "creating" && m.State != "removed") {
			return errors.New("invalid Pi fork member")
		}
		seen[c.Incarnation] = true
		if n > 0 {
			saved := team.Members[n-1]
			if saved.Address != c.Address || team.Roles[n] != c.Address || !backendScalar(saved.Role) || (saved.Access != "read_only" && saved.Access != "workspace_write") {
				return errors.New("Pi fork role changed")
			}
		}
		if m.State == "ready" {
			if m.Origin == nil || !piInitialInputID.MatchString(m.NativeID) || native[m.NativeID] || m.NativeID == m.Origin.Source.Conversation.NativeID || m.Origin.Source.Conversation.Session != f.Source || m.Origin.Source.Conversation.Member != c.Address || m.Origin.Source.Conversation.Incarnation != m.SourceIncarnation || m.Origin.Snapshot.Native.SourceNativeID != m.Origin.Source.Conversation.NativeID || m.Origin.Snapshot.Native.EntryCount < 1 || !isLowerHex(m.Origin.Snapshot.Native.EntriesDigest, 64) || m.Origin.Snapshot.Native.LeafID == "" || m.Origin.Snapshot.HistoryRef == "" {
				return errors.New("Pi fork lacks an independent native origin")
			}
			if c.Address == f.SelectedMember {
				selected = true
			}
			native[m.NativeID] = true
		} else if m.Origin != nil || m.NativeID != "" {
			return errors.New("unstarted or removed fork member has native authority")
		}
		if m.State == "creating" && (m.ReservedToolPolicy == nil || piruntime.ValidateToolPolicy(*m.ReservedToolPolicy) != nil) {
			return errors.New("unstarted Pi fork member lacks its tool ceiling")
		}
		if m.State != "creating" && m.ReservedToolPolicy != nil {
			return errors.New("Pi fork tool reservation has no unstarted member")
		}
		if m.State != "removed" && (!isLowerHex(m.CatalogDigest, 64) || m.PolicyRevision == 0 || m.Selection == nil || m.Selection.Revision == 0 || !slices.Contains([]string{"session_default", "member_override"}, m.SelectionSource)) {
			return errors.New("Pi fork policy provenance missing")
		}
	}
	if !selected || f.Members[0].State != "ready" {
		return errors.New("selected Pi fork conversation is not materialized")
	}
	active := i.ActiveConversations()
	if i.ProviderSelection.Schema != 1 || i.ProviderSelection.Policy.Public().Session != i.SessionRef() || i.ProviderSelection.Policy.Revision != 1 || i.ProviderSelection.Policy.Default.Revision != 1 || i.ProviderSelection.Policy.Overrides == nil || len(i.ProviderSelection.Members) != len(active) {
		return errors.New("invalid Pi fork destination policy")
	}
	for n, c := range active {
		desired := i.ProviderSelection.Members[n]
		if desired.Address != c.Address || desired.Incarnation != c.Incarnation || desired.Settings.Revision != 1 || !isLowerHex(desired.PolicyDigest, 64) || desired.Settings.PolicyRevision != desired.PolicyDigest {
			return errors.New("invalid Pi fork desired settings")
		}
		source, _ := i.ForkMember(c.Address)
		selected := i.ProviderSelection.Policy.Default
		for _, o := range i.ProviderSelection.Policy.Overrides {
			if o.Member == (piproviders.MemberKey{Address: c.Address, Incarnation: c.Incarnation}) {
				selected = o.Value
			}
		}
		if source.Selection == nil || !reflect.DeepEqual(source.Selection.Selection, selected.Selection) {
			return errors.New("Pi fork changed a saved selection chain")
		}
		if m, _ := i.ForkMember(c.Address); m.Origin != nil {
			a, d := m.Origin.Snapshot.Settings, desired.Settings.Effective()
			if a.Provider != d.Provider || a.AccountRef != d.AccountRef || a.Model != d.Model || a.Thinking != d.Thinking {
				return errors.New("Pi fork desired primary differs from captured native settings")
			}
			original := ""
			if c.Address == "lead" {
				original = team.LeadInstructions
			} else {
				for _, member := range team.Members {
					if member.Address == c.Address {
						original = member.Instructions
					}
				}
			}
			if m.Origin.Snapshot.Instructions != original {
				return errors.New("Pi fork instructions changed")
			}
		}
	}
	return nil
}

// Validate the union at the decoder boundary rather than defining UnmarshalJSON:
// the intent is embedded in a journal, whose fields must remain visible.
func validatePiCreationUnion(data []byte, schema int) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if schema == 5 {
		for _, key := range []string{"goal_sha256", "initial_input_id"} {
			if _, ok := fields[key]; ok {
				return errors.New("Pi fork cannot contain goal or input keys")
			}
		}
		if raw, ok := fields["fork"]; !ok || string(raw) == "null" {
			return errors.New("Pi fork origin missing")
		}
	} else {
		if len(data) > 64<<10 {
			return errors.New("Pi creation record exceeds bound")
		}
		if _, ok := fields["fork"]; ok {
			return errors.New("legacy Pi creation cannot contain fork data")
		}
	}
	return nil
}
func (i PiCreationIntent) RecordLimit() int {
	if i.Schema == 5 {
		return MaxPiForkRecordBytes
	}
	return 64 << 10
}
