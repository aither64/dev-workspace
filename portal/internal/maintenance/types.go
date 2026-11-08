// Package maintenance owns the private journal of one workspace profile update.
// Pause is bounded; interruption and resume retain the captured actor identity.
package maintenance

import (
	"encoding/json"
	"time"
)

const (
	Schema          = 2
	TransportSchema = 1
	MaxBytes        = 4 << 20
)

type Service struct {
	Workspace                  string `json:"workspace"`
	Unit                       string `json:"unit"`
	Native                     string `json:"native"`
	PID                        int    `json:"pid"`
	ProcessIdentity            string `json:"processIdentity"`
	Socket                     string `json:"socket"`
	SocketEpoch                string `json:"socketEpoch"`
	DestinationPID             int    `json:"destinationPid,omitempty"`
	DestinationProcessIdentity string `json:"destinationProcessIdentity,omitempty"`
	DestinationNative          string `json:"destinationNative,omitempty"`
	DestinationEpoch           string `json:"destinationEpoch,omitempty"`
}

type Child struct {
	Workspace string `json:"workspace"`
	Parent    string `json:"parent"`
	Thread    string `json:"thread"`
}

type Participant struct {
	Workspace               string `json:"workspace"`
	Slug                    string `json:"slug"`
	Incarnation             string `json:"incarnation"`
	Root                    string `json:"root"`
	Member                  string `json:"member,omitempty"`
	Thread                  string `json:"thread"`
	Cwd                     string `json:"cwd"`
	SourceTurn              string `json:"sourceTurn"`
	SocketEpoch             string `json:"socketEpoch"`
	NoticeID                string `json:"noticeId"`
	NoticeText              string `json:"noticeText"`
	NoticeAction            string `json:"noticeAction"`
	NoticeDisposition       string `json:"noticeDisposition"`
	NoticeRetirementIntent  bool   `json:"noticeRetirementIntent"`
	NoticeCleanupAccepted   bool   `json:"noticeCleanupAccepted"`
	NoticeRetired           bool   `json:"noticeRetired"`
	StopRevision            uint64 `json:"stopRevision"`
	ContinuationID          string `json:"continuationId"`
	ContinuationDisposition string `json:"continuationDisposition"`
	Reason                  string `json:"reason,omitempty"`
}

type Operation struct {
	PauseDeadline         time.Time          `json:"pauseDeadline"`
	Children              []Child            `json:"children"`
	SourcesStopped        bool               `json:"sourcesStopped"`
	Schema                int                `json:"schema"`
	OperationID           string             `json:"operationId"`
	Revision              uint64             `json:"revision"`
	Phase                 string             `json:"phase"`
	Profile               string             `json:"profile"`
	SourceProfileToken    string             `json:"sourceProfileToken"`
	SourcePackage         string             `json:"sourcePackage"`
	CandidatePackage      string             `json:"candidatePackage"`
	CreateDigest          string             `json:"createDigest,omitempty"`
	PlansDigest           string             `json:"plansDigest"`
	DrainedPortals        []string           `json:"drainedPortals,omitempty"`
	TerminalIntents       []TerminalIntent   `json:"terminalIntents,omitempty"`
	Preparation           json.RawMessage    `json:"preparation,omitempty"`
	ContinuationsSettled  bool               `json:"continuationsSettled"`
	NoticeCleanupComplete bool               `json:"noticeCleanupComplete"`
	Restoration           *RestorationIntent `json:"restoration,omitempty"`
	CancelRequested       bool               `json:"cancelRequested"`
	WorkerInvocation      string             `json:"workerInvocation,omitempty"`
	ActivationIntent      bool               `json:"activationIntent"`
	ActivationPackage     string             `json:"activationPackage,omitempty"`
	ActivationNative      string             `json:"activationNative,omitempty"`
	ActivationPlansDigest string             `json:"activationPlansDigest,omitempty"`
	SourceRecovered       bool               `json:"sourceRecovered"`
	Outcome               string             `json:"outcome,omitempty"`
	CutoverIntent         bool               `json:"cutoverIntent"`
	Services              []Service          `json:"services"`
	Participants          []Participant      `json:"participants"`
	Blockers              []string           `json:"blockers"`
}

func (o Operation) Terminal() bool { return o.Phase == "complete" || o.Phase == "cancelled" }

// TerminalIntent records only a managed client actually stopped by this operation.
type TerminalIntent struct {
	Workspace   string `json:"workspace"`
	Slug        string `json:"slug"`
	Root        string `json:"root"`
	Incarnation string `json:"incarnation"`
}

type Request struct {
	Schema           int                `json:"schema"`
	Action           string             `json:"action"`
	OperationID      string             `json:"operationId"`
	ThreadID         string             `json:"threadId,omitempty"`
	ExpectedRevision uint64             `json:"expectedRevision,omitempty"`
	Restoration      *RestorationIntent `json:"restoration,omitempty"`
	Operation        *Operation         `json:"operation,omitempty"`
	Startup          *StartupSelection  `json:"startup,omitempty"`
	PID              int                `json:"pid,omitempty"`
	Unit             string             `json:"unit,omitempty"`
	Invocation       string             `json:"invocation,omitempty"`
	Grant            *StartupGrant      `json:"grant,omitempty"`
	Slug             string             `json:"slug,omitempty"`
	MarkerSHA256     string             `json:"markerSHA256,omitempty"`
}

type Response struct {
	Schema    int               `json:"schema"`
	Status    string            `json:"status"`
	Reason    string            `json:"reason,omitempty"`
	Operation *Operation        `json:"operation,omitempty"`
	Startup   *StartupSelection `json:"startup,omitempty"`
}
