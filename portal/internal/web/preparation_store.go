package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"golang.org/x/sys/unix"
)

const (
	maxUnfinishedPreparations    = 512
	maxPreparationMappings       = 10000
	maxPreparationBytes          = 2*maxCreationReceiptBytes + 16384
	maxPreparationMappingBytes   = 2048
	maxPreparationAggregateBytes = maxUnfinishedPreparations*maxPreparationBytes + maxPreparationMappings*maxPreparationMappingBytes
)

// Presence belongs to submission identity, including empty optional fields.
type preparationValue struct {
	Present bool   `json:"present"`
	Value   string `json:"value"`
}

type preparationInput struct {
	RawPrompt   string           `json:"rawPrompt"`
	Date        string           `json:"date"`
	CustomName  string           `json:"customName"`
	UploadScope string           `json:"uploadScope"`
	Attachments []string         `json:"attachments"`
	Team        preparationValue `json:"team"`
	Catalog     preparationValue `json:"catalog"`
	Model       preparationValue `json:"model"`
	Effort      preparationValue `json:"effort"`
}

type preparationSnapshot struct {
	Input  preparationInput    `json:"input"`
	Preset *teamruntime.Preset `json:"preset,omitempty"`
	Goal   string              `json:"goal"`
}

// A terminal record is replaced atomically with a compact mapping, then moved
// to the completed directory. The CLI scans only the bounded unfinished dir;
// a crash between replacement and move leaves a valid compact record there.
type sessionPreparation struct {
	Schema         int                  `json:"schema"`
	Workspace      string               `json:"workspace"`
	RequestID      string               `json:"requestId"`
	InputVersion   int                  `json:"inputVersion"`
	InputDigest    string               `json:"inputDigest"`
	SnapshotDigest string               `json:"snapshotDigest,omitempty"`
	ReceiptID      string               `json:"receiptId"`
	Attempt        int                  `json:"attempt"`
	State          string               `json:"state"`
	Phase          string               `json:"phase"`
	Error          string               `json:"error,omitempty"`
	StartedAt      string               `json:"startedAt"`
	UpdatedAt      string               `json:"updatedAt"`
	Snapshot       *preparationSnapshot `json:"snapshot,omitempty"`
	Base           string               `json:"base,omitempty"`
	NamingOutcome  string               `json:"namingOutcome,omitempty"`
	Slug           string               `json:"slug,omitempty"`
	Epoch          string               `json:"epoch,omitempty"`
	Handoff        *creationRequest     `json:"handoff,omitempty"`
	Terminal       string               `json:"terminal,omitempty"`
}

func preparationDigest(value any) string {
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func inputPreparationDigest(input preparationInput) string {
	return preparationDigest(struct {
		Version int              `json:"version"`
		Input   preparationInput `json:"input"`
	}{1, input})
}

func (s *Server) preparationDirectory(completed bool) string {
	name := "session-preparations"
	if completed {
		name = "session-preparation-mappings"
	}
	return filepath.Join(s.operationStore.directory, name)
}

func (s *Server) preparationPath(id string, completed bool) string {
	return filepath.Join(s.preparationDirectory(completed), id+".json")
}

func validPreparationID(id string) bool {
	return queueClientMessageIDPattern.MatchString(id) && id[14] == '4' && strings.ContainsRune("89ab", rune(id[19]))
}

func validatePreparation(record sessionPreparation, workspace string) error {
	if record.Schema != 1 || record.Workspace != workspace || !validPreparationID(record.RequestID) || record.InputVersion != 1 ||
		!messageDigestPattern.MatchString(record.InputDigest) || !messageDigestPattern.MatchString(record.ReceiptID) || record.Attempt < 1 || len(record.Error) > 4096 {
		return errors.New("invalid preparation identity")
	}
	for _, value := range []string{record.StartedAt, record.UpdatedAt} {
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return errors.New("invalid preparation timestamp")
		}
	}
	if (record.Slug == "" && record.Epoch != "") || (record.Slug != "" && (!session.ValidSlug(record.Slug) || !messageDigestPattern.MatchString(record.Epoch))) {
		return errors.New("invalid preparation reservation")
	}
	if record.NamingOutcome != "" && record.NamingOutcome != "custom" && record.NamingOutcome != "model" &&
		record.NamingOutcome != "unavailable" && record.NamingOutcome != "utility_error" && record.NamingOutcome != "invalid_output" &&
		record.NamingOutcome != "timeout" && record.NamingOutcome != "attachment_only" {
		return errors.New("invalid preparation naming outcome")
	}
	if record.State == "terminal" {
		if record.Snapshot != nil || record.Handoff != nil || record.SnapshotDigest != "" || record.Base != "" || record.Slug == "" || record.Phase != "initializing" ||
			(record.Terminal != "ready" && record.Terminal != "conflict" && record.Terminal != "cancelled") {
			return errors.New("invalid preparation mapping")
		}
		return nil
	}
	if record.Terminal != "" {
		return errors.New("unfinished preparation has a terminal outcome")
	}
	if record.State != "accepting" && record.State != "running" && record.State != "paused" && record.State != "failed" && record.State != "handed_off" {
		return errors.New("invalid preparation state")
	}
	if record.Phase != "accepting" && record.Phase != "naming" && record.Phase != "reserving" && record.Phase != "initializing" && record.Phase != "stopped" {
		return errors.New("invalid preparation phase")
	}
	if record.Snapshot == nil || record.InputDigest != inputPreparationDigest(record.Snapshot.Input) ||
		record.SnapshotDigest != preparationDigest(record.Snapshot) {
		return errors.New("preparation snapshot digest mismatch")
	}
	input := record.Snapshot.Input
	if !utf8.ValidString(input.RawPrompt) || len(input.RawPrompt) > session.MaxMessageBytes || len(input.Attachments) > 10 ||
		(strings.TrimSpace(input.RawPrompt) == "" && len(input.Attachments) == 0) {
		return errors.New("invalid preparation prompt")
	}
	if _, err := creationDestination("session", input.Date); err != nil {
		return err
	}
	if input.CustomName != "" {
		if _, err := creationDestination(input.CustomName, input.Date); err != nil {
			return err
		}
	}
	for _, value := range []preparationValue{input.Team, input.Catalog, input.Model, input.Effort} {
		if (!value.Present && value.Value != "") || !utf8.ValidString(value.Value) || validateCreationSettingValue(value.Value) != nil {
			return errors.New("invalid preparation settings")
		}
	}
	seen := map[string]bool{}
	for _, id := range input.Attachments {
		if !queueClientMessageIDPattern.MatchString(id) || seen[id] {
			return errors.New("invalid preparation attachments")
		}
		seen[id] = true
	}
	if (len(input.Attachments) > 0 || input.UploadScope != "") && !queueClientMessageIDPattern.MatchString(input.UploadScope) {
		return errors.New("invalid preparation upload scope")
	}
	if record.Snapshot.Preset != nil && validDirectTeamPreset(*record.Snapshot.Preset) != nil {
		return errors.New("invalid preparation team snapshot")
	}
	if len(record.Snapshot.Goal) > session.MaxMessageBytes || !utf8.ValidString(record.Snapshot.Goal) {
		return errors.New("invalid preparation goal")
	}
	if (record.State == "handed_off" || record.State == "running" && record.Phase != "accepting") && record.Snapshot.Goal == "" {
		return errors.New("missing preparation goal")
	}
	if record.Base != "" && (!session.ValidSlug(record.Base) || len(record.Base) > 48) {
		return errors.New("invalid preparation base")
	}
	if (record.Slug == "") != (record.Handoff == nil) {
		return errors.New("incomplete preparation handoff")
	}
	if record.Slug != "" && (!strings.HasPrefix(record.Slug, input.Date+"-") || record.Base == "" ||
		(input.CustomName != "" && record.Slug != input.Date+"-"+input.CustomName)) {
		return errors.New("preparation destination does not match its frozen input")
	}
	if record.Handoff != nil {
		expected := preparationCreationRequest(record)
		if !sameCreationRequest(*record.Handoff, expected) {
			return errors.New("preparation handoff mismatch")
		}
	}
	return nil
}

func readPreparation(path, workspace string) (sessionPreparation, error) {
	var record sessionPreparation
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return record, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return record, err
	}
	stat, owned := info.Sys().(*syscall.Stat_t)
	if !owned || stat.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxPreparationBytes {
		return record, errors.New("preparation file must be bounded and private")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maxPreparationBytes+1))
	if err != nil {
		return record, err
	}
	if len(encoded) > maxPreparationBytes {
		return record, errors.New("preparation is too large")
	}
	if err := rejectDuplicateCreationJSONKeys(encoded); err != nil {
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return record, errors.New("trailing preparation data")
	}
	if record.State == "terminal" && len(encoded) > maxPreparationMappingBytes {
		return record, errors.New("preparation mapping is too large")
	}
	return record, validatePreparation(record, workspace)
}

func (s *Server) loadPreparations() error {
	s.preparations = make(map[string]sessionPreparation)
	s.preparationUnconfirmed = make(map[string]string)
	s.preparationWork = make(map[string]preparationWork)
	unfinished, totalBytes := 0, 0
	for _, completed := range []bool{false, true} {
		directory := s.preparationDirectory(completed)
		info, err := os.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		stat, owned := info.Sys().(*syscall.Stat_t)
		if !owned || stat.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode().Perm() != 0700 {
			return errors.New("preparation directory must be private")
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") && strings.HasSuffix(entry.Name(), ".tmp") {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			if !strings.HasSuffix(entry.Name(), ".json") || !validPreparationID(id) {
				return errors.New("invalid preparation filename")
			}
			if _, duplicate := s.preparations[id]; duplicate {
				return errors.New("duplicate preparation identity")
			}
			record, err := readPreparation(filepath.Join(directory, entry.Name()), s.config.Workspace)
			if err != nil {
				return fmt.Errorf("load preparation %s: %w", id, err)
			}
			if record.RequestID != id || (completed && record.State != "terminal") {
				return errors.New("preparation path identity mismatch")
			}
			if record.State != "terminal" {
				unfinished++
				totalBytes += maxPreparationBytes
			} else {
				totalBytes += maxPreparationMappingBytes
			}
			if len(s.preparations) >= maxPreparationMappings || unfinished > maxUnfinishedPreparations || totalBytes > maxPreparationAggregateBytes {
				return errors.New("preparation capacity exceeded")
			}
			if record.State == "running" {
				record.State, record.Phase = "paused", "stopped"
				record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
			s.preparations[id] = record
		}
	}
	return nil
}

var errPreparationPersistenceUnconfirmed = errors.New("preparation persistence is unconfirmed")

func (s *Server) syncPreparationDirectory(directory string) error {
	if s.preparationSync != nil {
		return s.preparationSync(directory)
	}
	return syncPreparationDirectory(directory)
}

// Only a real rename creates this obligation. Confirm the published bytes,
// never reconstruct them from an earlier snapshot after an uncertain write.
// The caller holds operationMu and a revalidated transition lock.
func (s *Server) confirmPreparationLocked(id string) error {
	path, pending := s.preparationUnconfirmed[id]
	if !pending {
		return nil
	}
	record := s.preparations[id]
	published, err := readPreparation(path, s.config.Workspace)
	if err == nil && preparationDigest(published) != preparationDigest(record) {
		err = errors.New("published preparation changed")
	}
	if err == nil {
		err = s.syncPreparationDirectory(s.operationStore.directory)
	}
	if err == nil {
		err = s.syncPreparationDirectory(filepath.Dir(path))
	}
	if err == nil && record.State == "terminal" {
		directory := s.preparationDirectory(true)
		if err = os.MkdirAll(directory, 0700); err == nil {
			err = privatePreparationDirectory(directory)
		}
		if err == nil {
			err = s.syncPreparationDirectory(s.operationStore.directory)
		}
		if err == nil && path != s.preparationPath(id, true) {
			err = os.Rename(path, s.preparationPath(id, true))
			if err == nil {
				path = s.preparationPath(id, true)
				s.preparationUnconfirmed[id] = path
			}
		}
		if err == nil {
			if _, statErr := os.Lstat(s.preparationDirectory(false)); statErr == nil {
				err = s.syncPreparationDirectory(s.preparationDirectory(false))
			} else if !errors.Is(statErr, os.ErrNotExist) {
				err = statErr
			}
		}
		if err == nil {
			err = s.syncPreparationDirectory(directory)
		}
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errPreparationPersistenceUnconfirmed, err)
	}
	delete(s.preparationUnconfirmed, id)
	return nil
}

func syncPreparationDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func privatePreparationDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	stat, owned := info.Sys().(*syscall.Stat_t)
	if !owned || stat.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("preparation directory must be private")
	}
	return nil
}

// The caller holds operationMu and a revalidated transition lock.
func (s *Server) savePreparation(record sessionPreparation) error {
	if err := s.confirmPreparationLocked(record.RequestID); err != nil {
		return err
	}
	if err := validatePreparation(record, s.config.Workspace); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	limit := maxPreparationBytes
	if record.State == "terminal" {
		limit = maxPreparationMappingBytes
	}
	if len(encoded) > limit {
		return errors.New("preparation record exceeds its byte limit")
	}
	directory := s.preparationDirectory(false)
	if previous, ok := s.preparations[record.RequestID]; ok && previous.State == "terminal" {
		if _, err := os.Lstat(s.preparationPath(record.RequestID, true)); err == nil {
			directory = s.preparationDirectory(true)
		}
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	if err := privatePreparationDirectory(directory); err != nil {
		return err
	}
	if err := s.syncPreparationDirectory(s.operationStore.directory); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".preparation-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(encoded); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	path := filepath.Join(directory, record.RequestID+".json")
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	// Rename has published the identity even if the following sync reports an
	// uncertain outcome. A same-process retry must not allocate another receipt.
	s.preparations[record.RequestID] = record
	if s.preparationUnconfirmed == nil {
		s.preparationUnconfirmed = make(map[string]string)
	}
	s.preparationUnconfirmed[record.RequestID] = path
	return s.confirmPreparationLocked(record.RequestID)
}

func (s *Server) preparationCapacity() error {
	unfinished := 0
	for _, record := range s.preparations {
		if record.State != "terminal" {
			unfinished++
		}
	}
	if unfinished >= maxUnfinishedPreparations || len(s.preparations) >= maxPreparationMappings {
		return errors.New("session request history is full")
	}
	return nil
}
