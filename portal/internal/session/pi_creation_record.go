package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
)

var piInitialInputID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// These are wire projections of the existing direct-team preset. No Codex
// thread/project identity is introduced for a Pi reserved conversation.
type PiCreationTeam struct {
	ID               string                 `json:"id"`
	Name             string                 `json:"name"`
	Description      string                 `json:"description"`
	Roles            []string               `json:"roles"`
	CatalogDigest    string                 `json:"catalogDigest"`
	TeamDigest       string                 `json:"teamDigest"`
	LeadModel        string                 `json:"leadModel"`
	LeadEffort       string                 `json:"leadEffort"`
	LeadInstructions string                 `json:"leadInstructions,omitempty"`
	Members          []PiCreationTeamMember `json:"members"`
}
type PiCreationTeamMember struct {
	Role         string `json:"role"`
	Address      string `json:"address"`
	Model        string `json:"model"`
	Effort       string `json:"reasoningEffort"`
	Behavior     string `json:"behavior"`
	Access       string `json:"access"`
	Purpose      string `json:"purpose,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}
type PiCreationIntent struct {
	Schema                int                      `json:"schema"`
	Workspace             string                   `json:"workspace"`
	Slug                  string                   `json:"slug"`
	Backend               agentbackend.Kind        `json:"backend"`
	BackendContract       int                      `json:"backend_contract"`
	ReceiptID             string                   `json:"receipt_id"`
	DeletionHistorySHA256 string                   `json:"deletion_history_sha256"`
	SessionIncarnation    string                   `json:"session_incarnation"`
	Conversations         []PiReservedConversation `json:"conversations"`
	GoalSHA256            string                   `json:"goal_sha256"`
	Model                 string                   `json:"model"`
	Effort                string                   `json:"effort"`
	Provider              string                   `json:"provider"`
	ProviderSelection     PiCreationSelection      `json:"provider_selection"`
	DirectTeam            PiCreationTeam           `json:"direct_team"`
	InitialInputID        string                   `json:"initial_input_id"`
	Sensitive             bool                     `json:"sensitive"`
}
type PiCreationNative struct {
	NativeID   string `json:"native_id"`
	HistoryRef string `json:"history_ref"`
}
type PiCreationJournal struct {
	PiCreationIntent
	State               string            `json:"state"`
	NativeCreateStarted bool              `json:"native_create_started"`
	NativeGeneration    string            `json:"native_generation,omitempty"`
	Native              *PiCreationNative `json:"native,omitempty"`
}

func (i PiCreationIntent) SessionRef() agentbackend.SessionRef {
	return agentbackend.SessionRef{Workspace: i.Workspace, Slug: i.Slug, Incarnation: i.SessionIncarnation}
}
func (i PiCreationIntent) Binding() BackendBinding {
	b := BackendBinding{Schema: 1, Backend: agentbackend.Pi, BackendContract: 1, Workspace: i.Workspace, Slug: i.Slug, SessionIncarnation: i.SessionIncarnation, CreationReceiptID: i.ReceiptID, Revision: 1, State: "creating", Conversations: []BackendConversation{}}
	for _, member := range i.Conversations {
		b.Conversations = append(b.Conversations, BackendConversation{Address: member.Address, Incarnation: member.Incarnation, State: "creating"})
	}
	return b
}
func (i PiCreationIntent) ValidateShape() error {
	if i.Schema != 4 || i.Backend != agentbackend.Pi || i.BackendContract != 1 {
		return agentbackend.ErrUnsupportedBinding
	}
	if !backendCanonicalPath(i.Workspace) || !backendSlug(i.Slug) || !isLowerHex(i.ReceiptID, 64) || !isLowerHex(i.DeletionHistorySHA256, 64) || !isLowerHex(i.SessionIncarnation, 64) || !isLowerHex(i.GoalSHA256, 64) || !piInitialInputID.MatchString(i.InitialInputID) || !backendScalar(i.Model) || !backendScalar(i.Effort) || !backendScalar(i.Provider) || len(i.Conversations) == 0 || len(i.Conversations) > 65 || i.Conversations[0].Address != "lead" {
		return errors.New("invalid Pi creation intent")
	}
	if err := i.Binding().Validate(i.Workspace, i.Slug); err != nil {
		return err
	}
	team := i.DirectTeam
	if !backendScalar(team.ID) || !backendScalar(team.Name) || !backendScalar(team.Description) || !isLowerHex(team.CatalogDigest, 64) || !isLowerHex(team.TeamDigest, 64) || team.Members == nil || len(team.Members)+1 != len(i.Conversations) || len(team.Roles) != len(i.Conversations) || team.Roles[0] != "lead" || team.LeadModel != i.Model || team.LeadEffort != i.Effort || len(i.ProviderSelection.Members) != len(i.Conversations) {
		return errors.New("Pi creation team disagrees with reserved identities")
	}
	if i.ProviderSelection.Schema != 1 || !isLowerHex(i.ProviderSelection.CatalogDigest, 64) || i.ProviderSelection.Policy.Public().Session != i.SessionRef() || i.ProviderSelection.Policy.Revision != 1 || i.ProviderSelection.Policy.Default.Revision != 1 || i.ProviderSelection.Policy.Overrides == nil {
		return errors.New("invalid Pi creation provider policy")
	}
	incarnations := map[string]bool{}
	for index, member := range i.Conversations {
		saved := i.ProviderSelection.Members[index]
		if incarnations[member.Incarnation] || saved.Address != member.Address || saved.Incarnation != member.Incarnation || !isLowerHex(saved.PolicyDigest, 64) || saved.Settings.PolicyRevision != saved.PolicyDigest || saved.Settings.Revision != 1 || !backendScalar(saved.Settings.Provider) || !backendScalar(saved.Settings.AccountRef) || !backendScalar(saved.Settings.Model) || !backendScalar(saved.Settings.Thinking) || (saved.Source != "session_default" && saved.Source != "member_override") {
			return errors.New("invalid frozen Pi member selection")
		}
		incarnations[member.Incarnation] = true
		if index == 0 {
			if saved.Settings.Provider != i.Provider || saved.Settings.Model != i.Model || saved.Settings.Thinking != i.Effort {
				return errors.New("Pi lead settings changed")
			}
			continue
		}
		preset := team.Members[index-1]
		if preset.Address != member.Address || team.Roles[index] != member.Address || !backendScalar(preset.Role) || preset.Model != saved.Settings.Model || preset.Effort != saved.Settings.Thinking || (preset.Access != "read_only" && preset.Access != "workspace_write") {
			return errors.New("Pi member preset settings changed")
		}
	}
	return nil
}
func (i PiCreationIntent) Validate(c piproviders.Catalog) error {
	if err := i.ValidateShape(); err != nil {
		return err
	}
	return i.ProviderSelection.Validate(c, i.SessionRef(), i.Conversations)
}
func DecodePiCreationIntent(data []byte) (PiCreationIntent, error) {
	var intent PiCreationIntent
	if err := DecodePiCreationJSON(data, &intent, 64<<10); err != nil {
		return intent, err
	}
	return intent, intent.ValidateShape()
}
func DecodePiCreationJournal(data []byte) (PiCreationJournal, error) {
	var journal PiCreationJournal
	if err := DecodePiCreationJSON(data, &journal, 64<<10); err != nil {
		return journal, err
	}
	if err := journal.PiCreationIntent.ValidateShape(); err != nil {
		return journal, err
	}
	if journal.State != "creating" && journal.State != "ready" {
		return journal, errors.New("invalid Pi creation journal state")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return journal, err
	}
	if !journal.NativeCreateStarted {
		if _, present := raw["native_generation"]; present {
			return journal, errors.New("Pi generation precedes create intent")
		}
	}
	if journal.NativeCreateStarted {
		if !isLowerHex(journal.NativeGeneration, 64) {
			return journal, errors.New("Pi creation intent lacks execution generation")
		}
	} else if journal.NativeGeneration != "" || journal.Native != nil {
		return journal, errors.New("Pi native result precedes create intent")
	}
	if journal.Native != nil && (!backendScalar(journal.Native.NativeID) || !backendScalar(journal.Native.HistoryRef)) {
		return journal, errors.New("invalid Pi native creation result")
	}
	if journal.State == "ready" && (!journal.NativeCreateStarted || journal.Native == nil) {
		return journal, errors.New("ready Pi journal lacks native identity")
	}
	return journal, nil
}
func (i PiCreationIntent) ValidateBinding(b BackendBinding) error {
	if err := b.Validate(i.Workspace, i.Slug); err != nil {
		return err
	}
	if b.CreationReceiptID != i.ReceiptID || b.SessionIncarnation != i.SessionIncarnation || len(b.Conversations) != len(i.Conversations) {
		return agentbackend.ErrBindingConflict
	}
	for n, member := range i.Conversations {
		if b.Conversations[n].Address != member.Address || b.Conversations[n].Incarnation != member.Incarnation {
			return fmt.Errorf("%w: reserved Pi member changed", agentbackend.ErrBindingConflict)
		}
	}
	return nil
}
