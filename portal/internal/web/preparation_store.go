package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
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
	Backend                     string           `json:"backend,omitempty"`
	PiProviderDigest            string           `json:"piProviderDigest,omitempty"`
	Sensitive                   bool             `json:"sensitive,omitempty"`
	RawPrompt                   string           `json:"rawPrompt"`
	Date                        string           `json:"date"`
	CustomName                  string           `json:"customName"`
	UploadScope                 string           `json:"uploadScope"`
	Attachments                 []string         `json:"attachments"`
	Team                        preparationValue `json:"team"`
	Catalog                     preparationValue `json:"catalog"`
	Model                       preparationValue `json:"model"`
	Effort                      preparationValue `json:"effort"`
	BrowserNotificationsEnabled *bool            `json:"browserNotificationsEnabled,omitempty"`
	Provider                    string           `json:"provider,omitempty"`
}

type preparationSnapshot struct {
	Input  preparationInput    `json:"input"`
	Preset *teamruntime.Preset `json:"preset,omitempty"`
	Goal   string              `json:"goal"`
	Pi     *preparationPi      `json:"pi,omitempty"`
}

type preparationPi struct {
	CatalogDigest string                `json:"catalogDigest"`
	Default       piproviders.Selection `json:"default"`
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
	version := 1
	if input.Backend == "pi" {
		version = 2
	}
	return preparationDigest(struct {
		Version int              `json:"version"`
		Input   preparationInput `json:"input"`
	}{version, input})
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
	if (record.Schema != 1 && record.Schema != 2) || record.Workspace != workspace || !validPreparationID(record.RequestID) || record.InputVersion != record.Schema ||
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
	if record.Schema == 1 {
		if input.Backend != "" || input.PiProviderDigest != "" || record.Snapshot.Pi != nil {
			return errors.New("mixed Codex/Pi preparation")
		}
	} else {
		if input.Backend != "pi" || !messageDigestPattern.MatchString(input.PiProviderDigest) || record.Snapshot.Pi == nil ||
			!messageDigestPattern.MatchString(record.Snapshot.Pi.CatalogDigest) || input.PiProviderDigest != piPreparationProviderDigest(*record.Snapshot.Pi) ||
			input.Provider != "" || input.UploadScope != "" || len(input.Attachments) != 0 || input.CustomName == "" ||
			!input.Team.Present || !input.Catalog.Present || !input.Model.Present || !input.Effort.Present || input.Model.Value == "" || input.Effort.Value == "" ||
			record.Snapshot.Preset == nil || requirePiSoloRoster(record.Snapshot.Preset.ID, record.Snapshot.Preset.Roles, len(record.Snapshot.Preset.Members)) != nil ||
			input.Team.Value != record.Snapshot.Preset.ID || input.Catalog.Value != record.Snapshot.Preset.CatalogDigest ||
			input.Model.Value != record.Snapshot.Preset.LeadModel || input.Effort.Value != record.Snapshot.Preset.LeadEffort ||
			validatePreparationPiDefault(record.Snapshot.Pi.Default) != nil {
			return errors.New("invalid Pi preparation snapshot")
		}
		if record.NamingOutcome != "" && record.NamingOutcome != "custom" {
			return errors.New("Pi preparation cannot use automatic naming")
		}
	}
	if input.Provider != "" && !codexproviders.ValidID(input.Provider) {
		return errors.New("preparation has an invalid provider")
	}
	if !utf8.ValidString(input.RawPrompt) || len(input.RawPrompt) > session.MaxMessageBytes || len(input.Attachments) > uploads.DefaultPromptFiles ||
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
	if record.Schema == 2 {
		// The new arm uses the owning strict codec (including null, exact
		// field spelling and numeric types); legacy decoding remains unchanged.
		if err := session.DecodePiCreationJSON(encoded, &record, maxPreparationBytes); err != nil {
			return record, err
		}
	} else {
		var envelope struct {
			Snapshot map[string]json.RawMessage `json:"snapshot"`
		}
		if err := json.Unmarshal(encoded, &envelope); err != nil {
			return record, err
		}
		for key := range envelope.Snapshot {
			if strings.EqualFold(key, "pi") {
				return record, errors.New("mixed Codex/Pi preparation fields")
			}
		}
		var input map[string]json.RawMessage
		for key, raw := range envelope.Snapshot {
			if strings.EqualFold(key, "input") {
				if err := json.Unmarshal(raw, &input); err != nil {
					return record, err
				}
			}
		}
		for key := range input {
			if strings.EqualFold(key, "backend") || strings.EqualFold(key, "piProviderDigest") {
				return record, errors.New("mixed Codex/Pi preparation fields")
			}
		}
	}
	if record.State == "terminal" && len(encoded) > maxPreparationMappingBytes {
		return record, errors.New("preparation mapping is too large")
	}
	if workspace != "" && record.Workspace != workspace {
		return record, errors.New("preparation workspace mismatch")
	}
	if !filepath.IsAbs(record.Workspace) || filepath.Clean(record.Workspace) != record.Workspace || strings.ContainsAny(record.Workspace, "\x00\n\r") || !utf8.ValidString(record.Workspace) {
		return record, errors.New("invalid preparation workspace")
	}
	return record, validatePreparation(record, record.Workspace)
}

// readPreparations is the single read-only traversal for boot and transition
// inspection. Publication, pausing and recovery remain owned by the Server.
func readPreparations(ctx context.Context, baseDirectory, workspace string) (map[string]sessionPreparation, error) {
	records := map[string]sessionPreparation{}
	unfinished, totalBytes := 0, 0
	for _, completed := range []bool{false, true} {
		directory := filepath.Join(baseDirectory, "session-preparations")
		if completed {
			directory = filepath.Join(filepath.Dir(directory), "session-preparation-mappings")
		}
		info, err := os.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		stat, owned := info.Sys().(*syscall.Stat_t)
		if !owned || stat.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode().Perm() != 0700 {
			return nil, errors.New("preparation directory must be private")
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if strings.HasPrefix(entry.Name(), ".") && strings.HasSuffix(entry.Name(), ".tmp") {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			if !strings.HasSuffix(entry.Name(), ".json") || !validPreparationID(id) {
				return nil, errors.New("invalid preparation filename")
			}
			if _, duplicate := records[id]; duplicate {
				return nil, errors.New("duplicate preparation identity")
			}
			record, err := readPreparation(filepath.Join(directory, entry.Name()), workspace)
			if err != nil {
				return nil, fmt.Errorf("load preparation %s: %w", id, err)
			}
			if record.RequestID != id || (completed && record.State != "terminal") {
				return nil, errors.New("preparation path identity mismatch")
			}
			if record.State != "terminal" {
				unfinished++
				totalBytes += maxPreparationBytes
			} else {
				totalBytes += maxPreparationMappingBytes
			}
			if len(records) >= maxPreparationMappings || unfinished > maxUnfinishedPreparations || totalBytes > maxPreparationAggregateBytes {
				return nil, errors.New("preparation capacity exceeded")
			}
			records[id] = record
		}
	}
	return records, nil
}

func (s *Server) loadPreparations() error {
	records, err := readPreparations(context.Background(), s.operationStore.directory, s.config.Workspace)
	if err != nil {
		return err
	}
	s.preparations = records
	s.preparationUnconfirmed = make(map[string]string)
	s.preparationWork = make(map[string]preparationWork)
	for id, record := range records {
		if record.State == "running" {
			record.State, record.Phase = "paused", "stopped"
			record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			s.preparations[id] = record
		}
	}
	return nil
}

// PiPreparationReference identifies a pre-root request, never a session/native
// authority. Compact schema-2 mappings retain this identity after retirement.
type PiPreparationReference struct {
	Workspace string `json:"workspace"`
	RequestID string `json:"requestId"`
}

// InspectRetainedPiPreparations inspects both authoritative preparation stores
// without constructing a Server, pausing records, or rewriting legacy state.
func InspectRetainedPiPreparations(ctx context.Context, stateRoot string, workspaces []string) ([]PiPreparationReference, error) {
	canonical := func(path string) bool {
		return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\n\r") && utf8.ValidString(path)
	}
	if !canonical(stateRoot) || len(workspaces) > 1024 {
		return nil, errors.New("invalid preparation inventory scope")
	}
	owners := map[string]string{}
	seen := map[string]bool{}
	for _, workspace := range workspaces {
		if !canonical(workspace) || seen[workspace] {
			return nil, errors.New("invalid preparation workspace")
		}
		seen[workspace] = true
		owners[filepath.Base(userstate.WorkspaceDirectory(stateRoot, "portal", workspace))] = workspace
	}
	result := []PiPreparationReference{}
	inventoried := 0
	base := filepath.Join(stateRoot, "portal")
	info, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	stat, owned := info.Sys().(*syscall.Stat_t)
	if !owned || stat.Uid != uint32(os.Geteuid()) || !info.IsDir() {
		return nil, errors.New("unsafe portal namespace")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		directory := filepath.Join(base, entry.Name())
		// Unrelated portal files are not preparation records.
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		present := false
		for _, name := range []string{"session-preparations", "session-preparation-mappings"} {
			if _, err := os.Lstat(filepath.Join(directory, name)); err == nil {
				present = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		if !present {
			continue
		}
		if err := privatePreparationDirectory(directory); err != nil {
			return nil, err
		}
		workspace := owners[entry.Name()]
		records, err := readPreparations(ctx, directory, workspace)
		if err != nil {
			return nil, err
		}
		inventoried += len(records)
		if inventoried > 65536 {
			return nil, errors.New("preparation inventory exceeds limit")
		}
		for id, record := range records {
			if filepath.Base(userstate.WorkspaceDirectory(stateRoot, "portal", record.Workspace)) != entry.Name() {
				return nil, errors.New("preparation namespace identity mismatch")
			}
			if record.Schema == 2 {
				if workspace == "" {
					return nil, errors.New("Pi preparation has no registered workspace")
				}
				result = append(result, PiPreparationReference{Workspace: workspace, RequestID: id})
			}
		}
		if len(result) > 65536 {
			return nil, errors.New("Pi preparation inventory exceeds limit")
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Workspace != result[j].Workspace {
			return result[i].Workspace < result[j].Workspace
		}
		return result[i].RequestID < result[j].RequestID
	})
	return result, nil
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
