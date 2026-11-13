package monitoroperation

import (
	"context"
	"time"
)

const ObserverSchema = 1

type LocalSafety struct {
	State        string   `json:"state"`
	Reason       string   `json:"reason,omitempty"`
	OperationIDs []string `json:"operationIds"`
}

type ArchiveAuthority struct {
	Slug         string `json:"slug"`
	RootThreadID string `json:"rootThreadId"`
	OperationID  string `json:"operationId"`
	ReceiptID    string `json:"receiptId,omitempty"`
	TargetID     string `json:"targetId"`
	Mode         string `json:"mode"`
}

type CISnapshot struct {
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion,omitempty"`
	ObservedAt   time.Time `json:"observedAt"`
	URL          string    `json:"url,omitempty"`
	SourceDigest string    `json:"-"` // Validated response binding, not persisted twice.
}

type PollReceipt struct {
	ID         string     `json:"id"`
	OwnerEpoch string     `json:"ownerEpoch"`
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"startedAt"`
	Handle     string     `json:"handle,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	ExitStatus *int       `json:"exitStatus,omitempty"`
}

type LocalProof struct {
	Kind     string    `json:"kind"`
	At       time.Time `json:"at"`
	Evidence []string  `json:"evidence"`
	Detail   string    `json:"detail"`
}

type ObserverRecord struct {
	Schema        int               `json:"schema"`
	ID            string            `json:"id"`
	Workspace     string            `json:"workspace"`
	Owner         Owner             `json:"owner"`
	Driver        string            `json:"driver"`
	RequestDigest string            `json:"requestDigest"`
	Source        Source            `json:"source"`
	SourceDigest  string            `json:"sourceDigest"`
	CreatedAt     time.Time         `json:"createdAt"`
	UpdatedAt     time.Time         `json:"updatedAt"`
	Revision      uint64            `json:"revision"`
	Phase         string            `json:"phase"`
	Remote        *CISnapshot       `json:"remote,omitempty"`
	Poll          *PollReceipt      `json:"poll,omitempty"`
	Archive       *ArchiveAuthority `json:"archive,omitempty"`
	LocalProof    *LocalProof       `json:"localProof,omitempty"`
	Events        []Event           `json:"events"`
	Detail        string            `json:"detail,omitempty"`
	CanceledAt    *time.Time        `json:"canceledAt,omitempty"`
}

type CIObserver interface {
	Poll(context.Context, Source) (CISnapshot, PollReceipt, error)
	Quiesce(context.Context, string) (PollReceipt, error)
}

func SourceDigest(source Source) string { return digest(source) }
