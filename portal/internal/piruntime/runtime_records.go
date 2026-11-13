package piruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// Schema 2 is the existing runtime.json with immutable request history. Entry
// schema and legacy schema-1 bytes remain unchanged; no second registry exists.
type runtimeRecords struct {
	Schema                       int
	Session                      agentbackend.SessionRef
	InstanceID, CurrentRequestID string
	Requests                     map[string]runtimeRecord
}

func (m *Manager) validateRuntimeRecord(record runtimeRecord, ref agentbackend.SessionRef) error {
	if record.Schema != 1 || record.Runtime.Session != ref || record.Start.Session != ref || !hexPattern.MatchString(record.RuntimeConfigSHA256) || record.HostStateRoot != m.config.HostStateRoot || record.AuthDirectory != m.config.AuthDirectory || !equalJSON(record.AccountBindings, m.config.AccountBindings) || (record.Phase != "preparing" && record.Phase != "starting") {
		return ErrScope
	}
	launch := launchFor(record.Start, record.Runtime)
	data, err := json.Marshal(launch)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, digest, err := DecodeLaunch(data)
	if err != nil || digest != record.Runtime.Digest || record.Runtime.RequestID != record.Start.RequestID || !uuidPattern.MatchString(record.Runtime.InstanceID) {
		return ErrScope
	}
	config, err := json.Marshal(m.runtimeConfiguration(launch, record.Runtime))
	if err != nil {
		return err
	}
	config = append(config, '\n')
	sum := sha256.Sum256(config)
	if hex.EncodeToString(sum[:]) != record.RuntimeConfigSHA256 {
		return ErrScope
	}
	return nil
}

func (m *Manager) records(ref agentbackend.SessionRef) (runtimeRecords, error) {
	var result runtimeRecords
	directory, err := stateDirectory(m.config.UserStateRoot, ref)
	if err != nil {
		return result, err
	}
	var raw json.RawMessage
	if err := readPrivate(filepath.Join(directory, "runtime.json"), &raw); err != nil {
		return result, err
	}
	var header struct{ Schema int }
	if json.Unmarshal(raw, &header) != nil {
		return result, ErrConflict
	}
	if header.Schema == 1 {
		var record runtimeRecord
		if err := strictDecode(raw, &record, recordLimit); err != nil {
			return result, err
		}
		if err := m.validateRuntimeRecord(record, ref); err != nil {
			return result, err
		}
		if record.Start.ReplacesRequestID != "" {
			return result, ErrConflict
		}
		return runtimeRecords{Schema: 1, Session: ref, InstanceID: record.Runtime.InstanceID, CurrentRequestID: record.Runtime.RequestID, Requests: map[string]runtimeRecord{record.Runtime.RequestID: record}}, nil
	}
	if header.Schema != 2 {
		return result, ErrConflict
	}
	if err := strictDecode(raw, &result, recordLimit); err != nil {
		return result, err
	}
	if result.Session != ref || !uuidPattern.MatchString(result.InstanceID) || !uuidPattern.MatchString(result.CurrentRequestID) || len(result.Requests) == 0 {
		return result, ErrScope
	}
	if _, ok := result.Requests[result.CurrentRequestID]; !ok {
		return result, ErrConflict
	}
	generations := map[string]bool{}
	for id, record := range result.Requests {
		if !uuidPattern.MatchString(id) || record.Runtime.RequestID != id || record.Runtime.InstanceID != result.InstanceID || m.validateRuntimeRecord(record, ref) != nil || generations[record.Runtime.Generation] {
			return runtimeRecords{}, ErrConflict
		}
		generations[record.Runtime.Generation] = true
		if record.Start.ReplacesRequestID != "" {
			previous, ok := result.Requests[record.Start.ReplacesRequestID]
			if !ok || previous.Runtime.RequestID == id || previous.Phase != "starting" {
				return runtimeRecords{}, ErrConflict
			}
		}
	}
	seen := map[string]bool{}
	for id := result.CurrentRequestID; id != ""; {
		if seen[id] {
			return runtimeRecords{}, ErrConflict
		}
		seen[id] = true
		record, ok := result.Requests[id]
		if !ok {
			return runtimeRecords{}, ErrConflict
		}
		id = record.Start.ReplacesRequestID
	}
	if len(seen) != len(result.Requests) {
		return runtimeRecords{}, ErrConflict
	}
	return result, nil
}

func (m *Manager) record(ref agentbackend.SessionRef) (runtimeRecord, error) {
	records, err := m.records(ref)
	if err != nil {
		return runtimeRecord{}, err
	}
	return records.Requests[records.CurrentRequestID], nil
}
func (m *Manager) recordFor(ref RuntimeRef) (runtimeRecord, bool, error) {
	records, err := m.records(ref.Session)
	if err != nil {
		return runtimeRecord{}, false, err
	}
	record, ok := records.Requests[ref.RequestID]
	if !ok || record.Runtime != ref {
		return runtimeRecord{}, false, ErrScope
	}
	return record, records.CurrentRequestID == ref.RequestID, nil
}

func (m *Manager) saveCurrentRecord(directory string, record runtimeRecord) error {
	records, err := m.records(record.Runtime.Session)
	if err != nil {
		// Only the initial reservation has no existing runtime record. Ordinary
		// callers already own runtime.lock and never replace malformed state.
		return err
	}
	old := records.Requests[records.CurrentRequestID]
	old.Phase = record.Phase
	if records.CurrentRequestID != record.Runtime.RequestID || !equalJSON(old, record) {
		return ErrConflict
	}
	if records.Schema == 1 {
		return writePrivate(filepath.Join(directory, "runtime.json"), record)
	}
	records.Requests[records.CurrentRequestID] = record
	return writePrivate(filepath.Join(directory, "runtime.json"), records)
}
