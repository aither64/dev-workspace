package web

import (
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type piCreationIdentity struct {
	BackendContract    int                              `json:"backendContract"`
	SessionIncarnation string                           `json:"sessionIncarnation"`
	Conversations      []session.PiReservedConversation `json:"conversations"`
	Provider           string                           `json:"provider"`
	InitialInputID     string                           `json:"initialInputId"`
	Selection          session.PiCreationSelection      `json:"selection"`
}

// This exact branch cannot inherit source/fork/Codex fields from the legacy
// request union, even if their zero values would otherwise disappear on replay.
type piCreationRequestWire struct {
	Kind                        string `json:"kind"`
	Slug                        string `json:"slug"`
	Goal                        string `json:"goal"`
	Backend                     string `json:"backend"`
	Sensitive                   bool   `json:"sensitive,omitempty"`
	Model                       string `json:"model,omitempty"`
	Effort                      string `json:"effort,omitempty"`
	Provider                    string `json:"provider,omitempty"`
	Team                        string `json:"team,omitempty"`
	CatalogDigest               string `json:"catalogDigest,omitempty"`
	BrowserNotificationsEnabled *bool  `json:"browserNotificationsEnabled,omitempty"`
}
type piCreationReceiptWire struct {
	Schema                int                    `json:"schema"`
	Workspace             string                 `json:"workspace"`
	Request               piCreationRequestWire  `json:"request"`
	ReceiptID             string                 `json:"receiptId"`
	DeletionHistorySHA256 string                 `json:"deletionHistorySha256"`
	Attempt               int                    `json:"attempt"`
	State                 string                 `json:"state"`
	Phase                 string                 `json:"phase"`
	Error                 string                 `json:"error,omitempty"`
	StartedAt             string                 `json:"startedAt"`
	UpdatedAt             string                 `json:"updatedAt"`
	Validated             bool                   `json:"validated"`
	Goal                  string                 `json:"goal"`
	Model                 string                 `json:"model"`
	Effort                string                 `json:"effort"`
	DirectTeam            session.PiCreationTeam `json:"directTeam"`
	Pi                    piCreationIdentity     `json:"pi"`
}

func validatePiCreationReceiptShape(raw map[string]json.RawMessage) error {
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var wire piCreationReceiptWire
	if err := session.DecodePiCreationJSON(data, &wire, maxCreationReceiptBytes); err != nil {
		return err
	}
	if wire.Schema != 4 || wire.Request.Backend != "pi" || wire.Request.Kind != "new" || wire.Attempt < 1 || !wire.Validated || wire.Goal != normalizedCreationGoal(wire.Request.Goal) || wire.Goal == "" || !validCreationRequestText(wire.Request.Goal, session.MaxMessageBytes) || len(wire.Goal) > session.MaxMessageBytes || wire.Phase == "" || len(wire.Phase) > 4096 || len(wire.Error) > 4096 {
		return errors.New("invalid Pi creation receipt")
	}
	if wire.State != "running" && wire.State != "paused" && wire.State != "failed" && wire.State != "ready" && wire.State != "conflict" {
		return errors.New("invalid Pi creation receipt state")
	}
	if _, err := time.Parse(time.RFC3339Nano, wire.StartedAt); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, wire.UpdatedAt); err != nil {
		return err
	}
	var receipt creationReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return err
	}
	if receipt.DirectTeam == nil || validDirectTeamPreset(*receipt.DirectTeam) != nil {
		return errors.New("invalid Pi direct team snapshot")
	}
	request, err := rawCreationObject(raw["request"])
	if err != nil {
		return err
	}
	if marker, present := request["sensitive"]; present && string(marker) != "true" {
		return errors.New("invalid Pi sensitivity marker")
	}
	if receipt.Request.Team != "" && receipt.Request.Team != receipt.DirectTeam.ID || receipt.Request.CatalogDigest != "" && receipt.Request.CatalogDigest != receipt.DirectTeam.CatalogDigest || receipt.Request.Model != "" && receipt.Request.Model != receipt.Model || receipt.Request.Effort != "" && receipt.Request.Effort != receipt.Effort || receipt.Request.Provider != "" && receipt.Request.Provider != receipt.Pi.Provider {
		return errors.New("Pi accepted request settings changed")
	}
	intent, err := receipt.piIntent()
	if err != nil {
		return err
	}
	return intent.ValidateShape()
}
func (r creationReceipt) piIntent() (session.PiCreationIntent, error) {
	if r.Schema != 4 || r.Pi == nil || r.DirectTeam == nil || r.Request.Backend != "pi" || r.Request.Kind != "new" || !r.Validated {
		return session.PiCreationIntent{}, errors.New("Pi creation receipt is incomplete")
	}
	data, err := json.Marshal(r.DirectTeam)
	if err != nil {
		return session.PiCreationIntent{}, err
	}
	var team session.PiCreationTeam
	if err := session.DecodePiCreationJSON(data, &team, 64<<10); err != nil {
		return session.PiCreationIntent{}, err
	}
	intent := session.PiCreationIntent{Schema: 4, Workspace: r.Workspace, Slug: r.Request.Slug, Backend: agentbackend.Pi, BackendContract: r.Pi.BackendContract, ReceiptID: r.ReceiptID, DeletionHistorySHA256: r.DeletionHistorySHA256, SessionIncarnation: r.Pi.SessionIncarnation, Conversations: r.Pi.Conversations, GoalSHA256: creationGoalDigest(r.Goal), Model: r.Model, Effort: r.Effort, Provider: r.Pi.Provider, ProviderSelection: r.Pi.Selection, DirectTeam: team, InitialInputID: r.Pi.InitialInputID, Sensitive: r.Request.Sensitive}
	return intent, intent.ValidateShape()
}
func piReceiptMatches(r creationReceipt, intent session.PiCreationIntent) bool {
	current, err := r.piIntent()
	return err == nil && reflect.DeepEqual(current, intent)
}
