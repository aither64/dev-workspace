package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
)

// Separate additive records leave the recovery and roster schemas readable by
// preceding generations. Records contain settings or counters, never messages.
func (s *Server) threadRecordPath(kind, thread string) string {
	digest := sha256.Sum256([]byte(thread))
	return filepath.Join(s.operationStore.directory, kind+"-"+hex.EncodeToString(digest[:])+".json")
}

func (s *Server) loadThreadRecord(kind, thread string, value any) error {
	data, err := s.readThreadRecord(kind, thread)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func (s *Server) readThreadRecord(kind, thread string) ([]byte, error) {
	file, err := os.Open(s.threadRecordPath(kind, thread))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16384 {
		return nil, errors.New("invalid private thread record")
	}
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Both keys are required in a frozen proof; null records proved absence.
// These are display/settings values, never receipts or execution admission.
type ProviderForkBrowserRecords struct {
	NextSettings  *userstate.DeferredSettings `json:"nextSettings"`
	ActivityFinal *codex.ActivitySnapshot     `json:"activityFinal"`
}

func (records *ProviderForkBrowserRecords) UnmarshalJSON(data []byte) error {
	type wire ProviderForkBrowserRecords
	if err := monitoroperation.DecodeMixedPiRecord(data, (*wire)(records), 32768); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) != 2 || fields["nextSettings"] == nil || fields["activityFinal"] == nil {
		return errors.New("frozen browser record fields are incomplete")
	}
	return nil
}

func FreezeProviderForkBrowserRecords(stateRoot, workspace, slug, root, thread string) (ProviderForkBrowserRecords, error) {
	store, err := newLifecycleOperationStore(workspace, stateRoot)
	if err != nil {
		return ProviderForkBrowserRecords{}, err
	}
	s := &Server{operationStore: store}
	var records ProviderForkBrowserRecords
	if data, err := s.readThreadRecord("next-settings", thread); err == nil {
		var value userstate.DeferredSettings
		if err := monitoroperation.DecodeMixedPiRecord(data, &value, 16384); err != nil {
			return records, err
		}
		if value.Slug != slug || value.Root != root || value.Thread != thread || (value.Model == "") != (value.Effort == "") || (value.RequestID == "") != (value.Epoch == "") {
			return records, errors.New("frozen next settings identity changed")
		}
		if value.RequestID != "" {
			return records, errors.New("original input settings are pending")
		}
		records.NextSettings = &value
	} else if !errors.Is(err, os.ErrNotExist) {
		return records, err
	}
	if data, err := s.readThreadRecord("activity-final", thread); err == nil {
		var value codex.ActivitySnapshot
		if err := monitoroperation.DecodeMixedPiRecord(data, &value, 16384); err != nil {
			return records, err
		}
		if value.ThreadID != thread || value.SentMessages < 0 || value.ReceivedMessages < 0 || value.TotalToolCalls < 0 || value.WorkingMS < 0 || value.WaitingMS < 0 || value.BetweenTurnsMS < 0 || value.UnclassifiedMS < 0 {
			return records, errors.New("frozen activity identity or counters changed")
		}
		records.ActivityFinal = &value
	} else if !errors.Is(err, os.ErrNotExist) {
		return records, err
	}
	return records, nil
}

// The lock owner supplies an exact frozen participant and known committed child.
// Conflicting destinations refuse; old records and receipt stores stay intact.
func ProjectProviderForkBrowserRecords(stateRoot, workspace, slug, root, source, child string, records ProviderForkBrowserRecords) error {
	if source == "" || child == "" || source == child {
		return errors.New("browser projection target is invalid")
	}
	store, err := newLifecycleOperationStore(workspace, stateRoot)
	if err != nil {
		return err
	}
	s := &Server{operationStore: store}
	next := records.NextSettings
	if next != nil {
		if next.Slug != slug || next.Root != root || next.Thread != source || (next.Model == "") != (next.Effort == "") || next.RequestID != "" || next.Epoch != "" {
			return errors.New("browser projection settings changed")
		}
		copy := *next
		copy.Thread = child
		next = &copy
	}
	activity := records.ActivityFinal
	if activity != nil {
		if activity.ThreadID != source {
			return errors.New("browser projection activity changed")
		}
		copy := *activity
		copy.ThreadID = child
		activity = &copy
	}
	for _, record := range []struct {
		kind     string
		expected any
		absent   bool
	}{{"next-settings", next, next == nil}, {"activity-final", activity, activity == nil}} {
		data, err := s.readThreadRecord(record.kind, child)
		if errors.Is(err, os.ErrNotExist) {
			if !record.absent {
				if err := s.saveThreadRecord(record.kind, child, record.expected); err != nil {
					return err
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		if record.absent {
			return errors.New("absent frozen browser record has conflicting child data")
		}
		var current any
		if record.kind == "next-settings" {
			current = &userstate.DeferredSettings{}
		} else {
			current = &codex.ActivitySnapshot{}
		}
		if err := monitoroperation.DecodeMixedPiRecord(data, current, 16384); err != nil {
			return err
		}
		if !reflect.DeepEqual(current, record.expected) {
			return errors.New("child browser projection conflicts with frozen value")
		}
	}
	return nil
}

// Recover the ONE predecessor display baseline from the immutable sealed source.
// Current activity-final may already be an aggregate and must never be its base.
func providerForkActivityBaseline(stateRoot, workspace string, current codexproviders.ResolvedBinding) (*codex.ActivitySnapshot, bool, error) {
	name := filepath.Base(filepath.Dir(current.Socket))
	if filepath.Base(current.Socket) != "app-server.sock" || !codexproviders.ValidID(name) {
		return nil, false, errors.New("activity binding socket identity changed")
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		return nil, false, err
	}
	state, err := codexproviders.ReadBindingState(stateRoot, name, workspace, current.Socket, catalog)
	if err != nil {
		return nil, false, err
	}
	resolved, err := state.ResolveCurrent(codexproviders.CurrentBinding{Key: current.Key, BindingRequired: true, ExpectedRevision: current.Revision, Provider: current.Provider, Socket: current.Socket, Eligible: true})
	if err != nil || resolved != current {
		return nil, false, errors.New("activity current binding changed")
	}
	targets, err := state.ReceiptTargets(current.Key, true, func(op string) ([]byte, error) { return codexproviders.ReadBindingEvidence(stateRoot, name, op) })
	if err != nil {
		return nil, false, err
	}
	if len(targets) == 1 {
		return nil, false, nil
	} // Original identity has no fork baseline.
	first := targets[0]
	sealBytes, err := codexproviders.ReadBindingEvidence(stateRoot, name, first.OperationID)
	if err != nil {
		return nil, false, err
	}
	sum := sha256.Sum256(sealBytes)
	if hex.EncodeToString(sum[:]) != first.EvidenceDigest {
		return nil, false, errors.New("activity evidence changed")
	}
	var seal codexproviders.BindingEvidence
	if err := json.Unmarshal(sealBytes, &seal); err != nil {
		return nil, false, err
	}
	verifiedBytes, err := codexproviders.ReadBindingSnapshot(stateRoot, name, first.OperationID, true)
	if err != nil {
		return nil, false, err
	}
	sum = sha256.Sum256(verifiedBytes)
	if hex.EncodeToString(sum[:]) != seal.SnapshotDigest {
		return nil, false, errors.New("activity verified snapshot changed")
	}
	var verified struct {
		OperationID          string `json:"operationId"`
		SourceSnapshotDigest string `json:"sourceSnapshotDigest"`
	}
	if err := json.Unmarshal(verifiedBytes, &verified); err != nil {
		return nil, false, err
	}
	if verified.OperationID != first.OperationID {
		return nil, false, errors.New("activity operation proof changed")
	}
	sourceBytes, err := codexproviders.ReadBindingSnapshot(stateRoot, name, first.OperationID, false)
	if err != nil {
		return nil, false, err
	}
	sum = sha256.Sum256(sourceBytes)
	if hex.EncodeToString(sum[:]) != verified.SourceSnapshotDigest {
		return nil, false, errors.New("activity frozen source changed")
	}
	var source struct {
		OperationID  string `json:"operationId"`
		Root         string `json:"root"`
		Socket       string `json:"socket"`
		Participants []struct {
			Current codexproviders.CurrentBinding `json:"current"`
			Thread  struct {
				ID string `json:"id"`
			} `json:"thread"`
			BrowserRecords *ProviderForkBrowserRecords              `json:"browserRecords"`
			History        struct{ Turns, Items []json.RawMessage } `json:"history"`
		} `json:"participants"`
	}
	if err := json.Unmarshal(sourceBytes, &source); err != nil {
		return nil, false, err
	}
	if source.OperationID != first.OperationID || source.Root != workspace || source.Socket != current.Socket {
		return nil, false, errors.New("activity frozen source ownership changed")
	}
	var baseline *codex.ActivitySnapshot
	empty, found := false, false
	for _, p := range source.Participants {
		if p.Current.Key != current.Key {
			continue
		}
		if found || p.Thread.ID != targets[1].ThreadID || p.BrowserRecords == nil || p.History.Turns == nil || p.History.Items == nil {
			return nil, false, errors.New("activity frozen participant changed")
		}
		found = true
		baseline = p.BrowserRecords.ActivityFinal
		empty = len(p.History.Turns) == 0 && len(p.History.Items) == 0
		if baseline != nil && baseline.ThreadID != p.Thread.ID {
			return nil, false, errors.New("activity frozen predecessor changed")
		}
	}
	if !found {
		return nil, false, errors.New("activity predecessor proof is missing")
	}
	return baseline, empty, nil
}

func (s *Server) saveThreadRecord(kind, thread string, value any) error {
	if err := s.operationStore.ensureDirectory(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 16384 {
		return errors.New("thread record exceeds limit")
	}
	file, err := os.CreateTemp(s.operationStore.directory, ".thread-record-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), s.threadRecordPath(kind, thread)); err != nil {
		return err
	}
	dir, err := os.Open(s.operationStore.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
