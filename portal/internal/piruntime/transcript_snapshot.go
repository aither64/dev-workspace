package piruntime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// Receipt identity belongs to the supervisor, never the native snapshot wire.
type acceptedInputWitness struct {
	EntryID     string `json:"entryId"`
	OperationID string `json:"operationId"`
	Digest      string `json:"digest"`
}

type transcriptSnapshot struct {
	Snapshot       workerSnapshot         `json:"snapshot"`
	AcceptedInputs []acceptedInputWitness `json:"acceptedInputs"`
}

func (s *transcriptSnapshot) UnmarshalJSON(data []byte) error {
	type plain transcriptSnapshot
	var value plain
	if err := strictDecode(data, &value, recordLimit); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 2 || value.AcceptedInputs == nil {
		return ErrScope
	}
	entries, operations := map[string]bool{}, map[string]bool{}
	for _, proof := range value.AcceptedInputs {
		if proof.EntryID == "" || !validString(proof.EntryID) || !operationPattern.MatchString(proof.OperationID) || !hexPattern.MatchString(proof.Digest) || entries[proof.EntryID] || operations[proof.OperationID] {
			return ErrScope
		}
		entries[proof.EntryID], operations[proof.OperationID] = true, true
	}
	*s = transcriptSnapshot(value)
	return nil
}

func (s transcriptSnapshot) transcript() backend.Transcript {
	result := projectTranscript(s.Snapshot)
	for i := range result.Entries {
		entry := &result.Entries[i]
		if entry.Kind == "userMessage" {
			for _, proof := range s.AcceptedInputs {
				if proof.EntryID == entry.ItemID && proof.Digest == MessageDigest(entry.Text) {
					entry.ClientUserMessageID, entry.ClientUserMessageDigest = proof.OperationID, proof.Digest
				}
			}
		}
	}
	return result
}

func decodeTranscriptSnapshotContract(data []byte) (bool, error) {
	var contract map[string]json.RawMessage
	if err := strictDecode(data, &contract, 1<<20); err != nil {
		return false, err
	}
	if contract == nil {
		return false, ErrScope
	}
	declaration, present := contract["piTranscriptSnapshot"]
	if !present {
		return false, nil
	}
	var version int
	if err := strictDecode(declaration, &version, 32); err != nil || version != 1 {
		return false, ErrScope
	}
	return true, nil
}

func readTranscriptSnapshotContract(app string) (bool, error) {
	if !storePattern.MatchString(app) {
		return false, ErrScope
	}
	file, err := os.Open(filepath.Join(app, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return false, err
	}
	return decodeTranscriptSnapshotContract(data)
}

// Select only from the validated current worker's frozen package. An operation
// or proof error never authorizes a second read through the legacy operation.
func (m *Manager) readTranscriptSnapshot(ctx context.Context, handle agentbackend.ExecutionHandle, before *int) (transcriptSnapshot, error) {
	var result transcriptSnapshot
	record, err := m.record(handle.Conversation.Session)
	if err != nil {
		return result, err
	}
	enriched, err := m.config.transcriptSnapshotContract(record.Start.AppStorePath)
	if err != nil {
		return result, err
	}
	operation, target := "snapshot", any(&result.Snapshot)
	if enriched {
		operation, target = "transcript_snapshot", &result
	}
	if err := m.handleCall(ctx, handle, operation, conversationRequest{Before: before}, target); err != nil {
		return transcriptSnapshot{}, err
	}
	if result.Snapshot.NativeID != handle.Conversation.NativeID {
		return transcriptSnapshot{}, ErrScope
	}
	return result, nil
}
