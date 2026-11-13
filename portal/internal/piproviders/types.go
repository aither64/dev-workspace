// Package piproviders resolves trusted Pi policy. It never sends model input,
// reads credentials, mutates Codex routing, or activates native fallback.
package piproviders

import (
	"context"
	"errors"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

var (
	ErrInvalidCatalog           = errors.New("invalid Pi provider catalog")
	ErrInvalidPolicy            = errors.New("invalid Pi provider policy")
	ErrUnavailableRoute         = errors.New("Pi provider route unavailable")
	ErrStalePolicy              = errors.New("stale Pi provider policy")
	ErrUnsupportedAccountSwitch = errors.New("Pi account switch unsupported")
	ErrHeld                     = piruntime.ErrHeld
)

type Provider struct {
	ID               string `json:"id"`
	Label            string `json:"label"`
	NativeID         string `json:"native_id"`
	RuntimeConfigRef string `json:"runtime_config_ref"`
}
type Account struct {
	Ref           string `json:"ref"`
	ProviderID    string `json:"provider_id"`
	CredentialRef string `json:"credential_ref"`
	CredentialKey string `json:"credential_key"`
	AuthKind      string `json:"auth_kind"`
}
type Model struct {
	Kind           string   `json:"kind"`
	ProviderID     string   `json:"provider_id"`
	ID             string   `json:"id"`
	Label          string   `json:"label"`
	ThinkingLevels []string `json:"thinking_levels"`
	Inputs         []string `json:"inputs"`
	Tools          bool     `json:"tools"`
	EvidenceRef    string   `json:"evidence_ref"`
}
type RouteGate struct {
	ProviderID  string `json:"provider_id"`
	AccountRef  string `json:"account_ref"`
	ModelID     string `json:"model_id"`
	State       string `json:"state"`
	ReasonCode  string `json:"reason_code"`
	EvidenceRef string `json:"evidence_ref"`
}
type Catalog struct {
	Schema    int         `json:"schema"`
	Digest    string      `json:"-"`
	Providers []Provider  `json:"providers"`
	Accounts  []Account   `json:"accounts"`
	Models    []Model     `json:"models"`
	Routes    []RouteGate `json:"routes"`
}
type Target struct {
	ProviderID string `json:"provider_id"`
	AccountRef string `json:"account_ref"`
	ModelID    string `json:"model_id"`
	Thinking   string `json:"thinking"`
}
type Budget struct {
	MaxExtraAttempts uint32 `json:"max_extra_attempts"`
	MaxHops          uint32 `json:"max_hops"`
}
type Selection struct {
	Chain      []Target          `json:"chain"`
	Budget     Budget            `json:"budget"`
	FallbackOn []FailureCategory `json:"fallback_on"`
}
type VersionedSelection struct {
	Revision  uint64    `json:"revision"`
	Selection Selection `json:"selection"`
}
type MemberKey struct {
	Address     string `json:"address"`
	Incarnation string `json:"incarnation"`
}
type Override struct {
	Member MemberKey          `json:"member"`
	Value  VersionedSelection `json:"value"`
}
type Policy struct {
	Session   agentbackend.SessionRef `json:"session"`
	Revision  uint64                  `json:"revision"`
	Default   VersionedSelection      `json:"default"`
	Overrides []Override              `json:"overrides"`
}
type Resolved struct {
	Session                             agentbackend.SessionRef
	Member                              MemberKey
	Requested                           Target
	Settings                            piruntime.EffectiveSettings
	Chain                               []Target
	Budget                              Budget
	FallbackOn                          []FailureCategory
	Source, CatalogDigest, PolicyDigest string
	provenance                          *resolvedProvenance
}

// resolvedProvenance is process-local validation evidence, not authority or a
// persisted execution receipt. Its snapshot never shares exported mutable slices.
type resolvedProvenance struct{ value Resolved }
type Configurer interface {
	Configure(context.Context, agentbackend.ExecutionHandle, piruntime.EffectiveSettings) (piruntime.SettingsResult, error)
}
type FailureCategory string

const (
	Configuration    FailureCategory = "configuration"
	Authentication   FailureCategory = "authentication"
	RefreshStorage   FailureCategory = "refresh_storage"
	ModelUnavailable FailureCategory = "model_unavailable"
	RateLimit        FailureCategory = "rate_limit"
	Overload         FailureCategory = "overload"
	QuotaBilling     FailureCategory = "quota_billing"
	Transport        FailureCategory = "transport"
	StructuredPolicy FailureCategory = "structured_policy"
	ContextLimit     FailureCategory = "context_limit"
	TextRefusal      FailureCategory = "text_refusal"
	Cancelled        FailureCategory = "cancelled"
	Unknown          FailureCategory = "unknown"
)

type Failure struct {
	Stage                           string
	Category                        FailureCategory
	Code, EvidenceKind, EvidenceRef string
	HTTPStatus                      int
	ObservedAt                      time.Time
}
type AttemptEvidence struct {
	Submission                               piruntime.SubmissionRef
	PolicyDigest                             string
	Ordinal, ChainIndex, ExtraAttempts, Hops uint32
	Actual                                   *Target
	NativeRequestID, NativeEntryID           string
	SubmissionOutcome, Effects               string
	PartialOutput                            bool
	Failure                                  Failure
}
type FallbackDecision struct {
	Outcome, Reason     string
	Next                *Target
	ExtraAttempts, Hops uint32
}

// PublicCatalog omits all runtime and credential references.
type PublicCatalog struct {
	Providers []ProviderSummary `json:"providers"`
	Models    []ModelSummary    `json:"models"`
	Routes    []RouteSummary    `json:"routes"`
}
type ProviderSummary struct{ ID, Label string }
type ModelSummary struct {
	ProviderID, ID, Label string
	ThinkingLevels        []string
	Inputs                []string
	Tools                 bool
}
type RouteSummary struct{ ProviderID, AccountRef, ModelID, State, ReasonCode string }
type DispatchObservation struct {
	Provider, Model, API, Thinking string
	ConfiguredAccountRef           string
	ObservedAt                     time.Time
}
type SettingsView struct {
	Source       string
	Requested    Target
	Chain        []Target
	PolicyDigest string
	Effective    *piruntime.EffectiveSettings
	Physical     *DispatchObservation
}
