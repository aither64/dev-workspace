// Package monitoroperation owns transient verification utilities and their
// durable outcomes. Utilities are never retained team members.
package monitoroperation

import (
	"context"
	"time"

	"github.com/aither64/codex-web/codex"
)

const Schema = 1

type Owner struct {
	Slug          string `json:"slug"`
	RootThreadID  string `json:"rootThreadId"`
	Incarnation   string `json:"incarnation"`
	OriginTurnID  string `json:"originTurnId"`
	Socket        string `json:"socket"`
	SocketEpoch   string `json:"socketEpoch"`
	Provider      string `json:"provider,omitempty"`
	RuntimeSchema int    `json:"runtimeSchema"`
}

// Source identifies one invocation or CI run attempt, never a moving branch.
// EnvironmentReferences name nonsecret inputs; they must not contain values.
type Source struct {
	Kind                  string   `json:"kind"`
	Argv                  []string `json:"argv,omitempty"`
	Cwd                   string   `json:"cwd,omitempty"`
	InputIdentity         string   `json:"inputIdentity,omitempty"`
	Revision              string   `json:"revision,omitempty"`
	InvocationID          string   `json:"invocationId,omitempty"`
	EnvironmentReferences []string `json:"environmentReferences,omitempty"`
	Provider              string   `json:"provider,omitempty"`
	Repository            string   `json:"repository,omitempty"`
	WorkflowID            string   `json:"workflowId,omitempty"`
	RunID                 string   `json:"runId,omitempty"`
	RunAttempt            int      `json:"runAttempt,omitempty"`
	Commit                string   `json:"commit,omitempty"`
}

type StartRequest struct {
	ID                string `json:"id"`
	Brief             string `json:"brief"`
	Source            Source `json:"source"`
	CancellationScope string `json:"cancellationScope,omitempty"`
}

type UtilityPolicy struct {
	PackageRoot    string `json:"packageRoot"`
	CatalogDigest  string `json:"catalogDigest"`
	NativeName     string `json:"nativeName"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	Instructions   string `json:"instructions"`
	Sandbox        string `json:"sandbox"`
	ApprovalPolicy string `json:"approvalPolicy"`
	MaxConcurrent  int    `json:"maxConcurrent"`
}

type Launch struct {
	State              string             `json:"state"`
	ProjectID          string             `json:"projectId,omitempty"`
	ThreadID           string             `json:"threadId,omitempty"`
	CreateAttempted    bool               `json:"createAttempted"`
	BootstrapAttempted bool               `json:"bootstrapAttempted"`
	BootstrapMarker    string             `json:"bootstrapMarker,omitempty"`
	MessageID          string             `json:"messageId"`
	Envelope           string             `json:"envelope"`
	Options            codex.TurnOptions  `json:"options"`
	Receipt            *codex.SendReceipt `json:"receipt,omitempty"`
	ActivatedEpoch     string             `json:"activatedEpoch,omitempty"`
	TerminatedAt       *time.Time         `json:"terminatedAt,omitempty"`
	Termination        string             `json:"termination,omitempty"`
	Detail             string             `json:"detail,omitempty"`
}

type ReportRequest struct {
	Slot          string   `json:"slot"`
	Outcome       string   `json:"outcome"`
	ExitStatus    *int     `json:"exitStatus,omitempty"`
	Conclusion    string   `json:"conclusion,omitempty"`
	ElapsedMillis int64    `json:"elapsedMillis"`
	Evidence      []string `json:"evidence,omitempty"`
	StillRunning  string   `json:"stillRunning,omitempty"`
	Detail        string   `json:"detail,omitempty"`
}

type Delivery struct {
	Status        string             `json:"status"`
	MessageID     string             `json:"messageId"`
	Envelope      string             `json:"envelope"`
	Context       string             `json:"context"`
	Options       *codex.TurnOptions `json:"options,omitempty"`
	Receipt       *codex.SendReceipt `json:"receipt,omitempty"`
	AdmittedEpoch string             `json:"admittedEpoch,omitempty"`
	Detail        string             `json:"detail,omitempty"`
}

type Event struct {
	ID        string        `json:"id"`
	CreatedAt time.Time     `json:"createdAt"`
	Report    ReportRequest `json:"report"`
	Delivery  Delivery      `json:"delivery"`
}

type Operation struct {
	Schema            int           `json:"schema"`
	ID                string        `json:"id"`
	Workspace         string        `json:"workspace"`
	Owner             Owner         `json:"owner"`
	Source            Source        `json:"source"`
	Brief             string        `json:"brief"`
	CancellationScope string        `json:"cancellationScope,omitempty"`
	RequestDigest     string        `json:"requestDigest"`
	UtilityPolicy     UtilityPolicy `json:"utilityPolicy"`
	CreatedAt         time.Time     `json:"createdAt"`
	Launch            Launch        `json:"launch"`
	Events            []Event       `json:"events"`
	CanceledAt        *time.Time    `json:"canceledAt,omitempty"`
	Disposition       string        `json:"disposition,omitempty"`
	Resolution        *Resolution   `json:"resolution,omitempty"`
}

type ResolutionRequest struct {
	OperationID  string   `json:"operationId"`
	ResolutionID string   `json:"resolutionId"`
	Evidence     []string `json:"evidence"`
	Detail       string   `json:"detail"`
}

type Resolution struct {
	ID           string    `json:"id"`
	At           time.Time `json:"at"`
	LeadThreadID string    `json:"leadThreadId"`
	Evidence     []string  `json:"evidence"`
	Detail       string    `json:"detail"`
}

type EventRef struct{ OperationID, EventID string }

type OwnedThread struct {
	OperationID string               `json:"operationId"`
	ProjectID   string               `json:"projectId"`
	ThreadID    string               `json:"threadId"`
	Cwd         string               `json:"cwd"`
	Settings    codex.ThreadSettings `json:"settings"`
	Disposition string               `json:"disposition,omitempty"`
}

type Inventory struct {
	Threads    []OwnedThread `json:"threads"`
	Unresolved []string      `json:"unresolved"`
	Blockers   []string      `json:"blockers"`
}

type LaunchClient interface {
	CreateProject(context.Context, string, string) (codex.ProjectMetadata, error)
	ReadProject(context.Context, string) (codex.ProjectMetadata, error)
	ListThreads(context.Context, codex.ThreadListOptions) ([]codex.ThreadMetadata, *string, error)
	StartThreadWithSettings(context.Context, string, map[string]string, codex.ThreadSettings) (string, error)
	ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
	BootstrapHeadlessThread(context.Context, string, string, string, string) error
	VerifyHeadlessBootstrap(context.Context, string, string, string, string) error
	SendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, error)
	ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error)
}

type Service struct {
	ObserverEpoch   string
	Store           *Store
	Client          LaunchClient
	ResolveLead     func(context.Context, string) (Owner, error)
	Resolve         func(string) (Owner, error)
	LockSession     func(context.Context, string) (func(), error)
	Policy          func(context.Context, Owner) (UtilityPolicy, error)
	Activate        func(context.Context, Owner, string) error
	Admit           func(context.Context, Owner) error
	Now             func() time.Time
	CheckResolution func(context.Context, Operation) error
}
