package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const maxLifecycleJournalBytes = 64 * 1024

// CodexTargets is immutable selection owned by the first lifecycle receipt,
// prelude or journal. Absence remains an accepted legacy choice forever.
type CodexTargets struct {
	Incarnation string              `json:"incarnation"`
	Revision    uint64              `json:"revision"`
	ThreadID    string              `json:"threadId"`
	Members     []CodexTargetMember `json:"members"`
}
type CodexTargetMember struct {
	Member           string  `json:"member"`
	OriginalThreadID *string `json:"originalThreadId"`
	ThreadID         *string `json:"threadId"`
}

func (targets CodexTargets) Validate(allowUnstarted bool) error {
	validThread := func(id string) bool { return id != "" && len(id) <= 512 && !strings.ContainsAny(id, "\x00\t\r\n ") }
	if !validLifecycleJournalID(targets.Incarnation) || targets.Revision == 0 || !validThread(targets.ThreadID) || targets.Members == nil {
		return errors.New("invalid captured Codex lifecycle identity")
	}
	originals, native := map[string]bool{}, map[string]bool{targets.ThreadID: true}
	prior := ""
	for _, m := range targets.Members {
		if !codexproviders.ValidID(m.Member) || m.Member == "lead" || m.Member <= prior {
			return errors.New("captured Codex members are not unique and sorted")
		}
		prior = m.Member
		if m.OriginalThreadID == nil || m.ThreadID == nil {
			if !allowUnstarted || m.OriginalThreadID != nil || m.ThreadID != nil {
				return errors.New("captured Codex member has no exact paired identity")
			}
			continue
		}
		if !validThread(*m.OriginalThreadID) || !validThread(*m.ThreadID) || originals[*m.OriginalThreadID] || native[*m.ThreadID] {
			return errors.New("duplicate or invalid captured Codex member identity")
		}
		originals[*m.OriginalThreadID], native[*m.ThreadID] = true, true
	}
	data, err := json.Marshal(targets)
	if err != nil || len(data) > maxLifecycleJournalBytes {
		return errors.New("captured Codex targets exceed 64 KiB")
	}
	return nil
}
func (targets *CodexTargets) Clone() *CodexTargets {
	if targets == nil {
		return nil
	}
	copy := *targets
	if targets.Members != nil {
		copy.Members = make([]CodexTargetMember, len(targets.Members))
	}
	for i, m := range targets.Members {
		copy.Members[i] = m
		if m.OriginalThreadID != nil {
			value := *m.OriginalThreadID
			copy.Members[i].OriginalThreadID = &value
		}
		if m.ThreadID != nil {
			value := *m.ThreadID
			copy.Members[i].ThreadID = &value
		}
	}
	return &copy
}
func SameCodexTargets(a, b *CodexTargets) bool { return reflect.DeepEqual(a, b) }
func DecodeCodexTargets(data []byte) (CodexTargets, error) {
	var result CodexTargets
	if len(data) == 0 || len(data) > maxLifecycleJournalBytes || !utf8.Valid(data) {
		return result, errors.New("invalid captured Codex target bytes")
	}
	scan := json.NewDecoder(bytes.NewReader(data))
	scan.UseNumber()
	if err := checkBackendJSONValue(scan, 0, true); err != nil {
		return result, err
	}
	if _, err := scan.Token(); !errors.Is(err, io.EOF) {
		return result, errors.New("trailing captured Codex targets")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return result, err
	}
	if len(fields) != 4 || len(fields["incarnation"]) == 0 || len(fields["revision"]) == 0 || len(fields["threadId"]) == 0 || len(fields["members"]) == 0 {
		return result, errors.New("incomplete captured Codex targets")
	}
	type plain CodexTargets
	var decoded plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return result, err
	}
	var members []map[string]json.RawMessage
	if err := json.Unmarshal(fields["members"], &members); err != nil {
		return result, err
	}
	if members == nil {
		return result, errors.New("captured Codex members must be explicit")
	}
	for _, m := range members {
		if len(m) != 3 || len(m["member"]) == 0 || len(m["originalThreadId"]) == 0 || len(m["threadId"]) == 0 {
			return result, errors.New("incomplete captured Codex member")
		}
	}
	result = CodexTargets(decoded)
	return result, result.Validate(true)
}
func (targets *CodexTargets) UnmarshalJSON(data []byte) error {
	value, err := DecodeCodexTargets(data)
	if err != nil {
		return err
	}
	*targets = value
	return nil
}

// Decode only the existing top-level capture with its nullable deletion tuple.
// Other fields retain the ordinary backend reader's recursive null refusal.
func decodeLifecycleObject(data []byte, maximum int) (map[string]json.RawMessage, *CodexTargets, error) {
	fields, err := readLifecycleObject(data, maximum)
	if err != nil {
		return nil, nil, err
	}
	targets, err := validateLifecycleFields(fields)
	return fields, targets, err
}

func readLifecycleObject(data []byte, maximum int) (map[string]json.RawMessage, error) {
	if len(data) == 0 || len(data) > maximum || !utf8.Valid(data) {
		return nil, errors.New("invalid lifecycle record size or encoding")
	}
	scan := json.NewDecoder(bytes.NewReader(data))
	scan.UseNumber()
	if err := checkBackendJSONValue(scan, 0, true); err != nil {
		return nil, err
	}
	if _, err := scan.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing lifecycle JSON")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, errors.New("lifecycle record must be one object")
	}
	return fields, nil
}

func validateLifecycleFields(fields map[string]json.RawMessage) (*CodexTargets, error) {
	var targets *CodexTargets
	for name, raw := range fields {
		if name == "codex_targets" {
			value, err := DecodeCodexTargets(raw)
			if err != nil {
				return nil, err
			}
			targets = &value
			continue
		}
		if err := checkBackendJSON(json.NewDecoder(bytes.NewReader(raw)), 0); err != nil {
			return nil, err
		}
	}
	if _, pi := fields["pi"]; pi && targets != nil {
		return nil, errors.New("Pi lifecycle cannot carry Codex targets")
	}
	return targets, nil
}

type LifecycleCommandInput struct {
	OperationID string        `json:"operation_id"`
	Targets     *CodexTargets `json:"codex_targets,omitempty"`
}

// This projection supplies expected data only. Command owners separately prove
// operation, phase, package, locks and deletion eligibility before any effects.
func DecodeLifecycleCommandInput(data []byte) (LifecycleCommandInput, error) {
	fields, targets, err := decodeLifecycleObject(data, maxLifecycleJournalBytes)
	if err != nil {
		return LifecycleCommandInput{}, err
	}
	for name := range fields {
		if name != "operation_id" && name != "codex_targets" {
			return LifecycleCommandInput{}, errors.New("unknown lifecycle command field")
		}
	}
	var operationID string
	if err := json.Unmarshal(fields["operation_id"], &operationID); err != nil || !validLifecycleJournalID(operationID) {
		return LifecycleCommandInput{}, errors.New("lifecycle command operation identity is missing")
	}
	return LifecycleCommandInput{OperationID: operationID, Targets: targets}, nil
}

// ValidateDeletionJournalTargets checks only the existing envelope/capture.
// The deletion owner must still validate its complete journal, authority,
// phase, original root and unstarted eligibility from these same bytes.
func ValidateDeletionJournalTargets(data []byte) (*CodexTargets, error) {
	_, targets, err := decodeLifecycleObject(data, maxLifecycleJournalBytes)
	return targets, err
}

type LifecycleProgress struct {
	CodexTargets           *CodexTargets `json:"-"`
	ServicePrelude         bool          `json:"-"`
	ServicePreludeEvidence string        `json:"-"`
	Operation              string        `json:"operation"`
	Phase                  string        `json:"phase"`
	Mode                   string        `json:"mode,omitempty"`
	Force                  bool          `json:"force,omitempty"`
	JournalID              string        `json:"journalId,omitempty"`
	Evidence               string        `json:"-"`
	RetainedThreadID       *string       `json:"-"`
	RootRecorded           bool          `json:"-"`
	UpdatedAt              time.Time     `json:"updatedAt"`
}

// PendingLifecycles discovers lifecycle journals, including deletions whose
// tracked session directory has already moved to private recovery storage.
func PendingLifecycles(workspace string) (map[string]LifecycleProgress, error) {
	root := filepath.Join(workspace, "worktrees", ".locks")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]LifecycleProgress{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect lifecycle journal directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("lifecycle journal directory is unsafe")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read lifecycle journal directory: %w", err)
	}
	candidates := make(map[string]struct{})
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), serviceCleanupSuffix) {
			slug := strings.TrimSuffix(entry.Name(), serviceCleanupSuffix)
			if ValidSlug(slug) {
				candidates[slug] = struct{}{}
			}
		}
		for _, journal := range LifecycleJournals() {
			suffix := "." + journal.Name + ".json"
			if strings.HasSuffix(entry.Name(), suffix) {
				slug := strings.TrimSuffix(entry.Name(), suffix)
				if ValidSlug(slug) {
					candidates[slug] = struct{}{}
				}
			}
		}
	}
	result := make(map[string]LifecycleProgress, len(candidates))
	for slug := range candidates {
		progress, progressErr := PendingLifecycleProgress(workspace, slug)
		if progressErr != nil {
			return nil, progressErr
		}
		if progress != nil {
			result[slug] = *progress
		}
	}
	return result, nil
}

var lifecyclePhases = map[string]map[string]struct{}{
	"archive": phaseSet(
		"prepared", "quiesced", "clusters_released", "tracking_archived",
		"tracking_committed", "tracking_saved", "thread_retired", "runtime_retired", "archived",
	),
	"delete": phaseSet(
		"prepared", "validated", "thread_retiring", "thread_retired",
		"clusters_released", "worktrees_removed", "runtime_retired",
		"tracking_preserved", "tracking_committed", "removed",
	),
	"revive": phaseSet(
		"prepared", "tracking_restored", "tracking_committed", "tracking_saved", "runtime_starting",
		"runtime_started", "revived",
	),
}

// ValidLifecyclePhase exposes the existing journal vocabulary to read-only
// projections without duplicating or extending lifecycle command mappings.
func ValidLifecyclePhase(operation, phase string) bool {
	_, ok := lifecyclePhases[operation][phase]
	return ok
}

func phaseSet(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

// PendingLifecycle returns the high-level operation that owns a session slug.
// Unsafe or conflicting journal state is reported as an error so browser
// mutations fail closed until the matching CLI operation repairs it.
func PendingLifecycle(workspace, slug string) (string, error) {
	if !ValidSlug(slug) {
		return "", errors.New("invalid session slug")
	}
	root := filepath.Join(workspace, "worktrees", ".locks")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect lifecycle journal directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("lifecycle journal directory is unsafe")
	}

	operation := ""
	for _, journal := range LifecycleJournals() {
		path := filepath.Join(root, slug+"."+journal.Name+".json")
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect lifecycle journal: %w", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !ok ||
			stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o600 ||
			info.Size() > 64*1024 {
			return "", fmt.Errorf("lifecycle journal is unsafe: %s", path)
		}
		if operation != "" && operation != journal.Command {
			return "", fmt.Errorf("conflicting lifecycle journals reserve session %s", slug)
		}
		operation = journal.Command
	}
	prelude, err := readServiceCleanupProgress(workspace, slug)
	if err != nil {
		return "", err
	}
	if prelude != nil {
		if operation != "" {
			if _, err := PendingLifecycleProgress(workspace, slug); err != nil {
				return "", err
			}
		}
		if operation != "" && operation != prelude.Operation {
			return "", fmt.Errorf("conflicting lifecycle journals reserve session %s", slug)
		}
		operation = prelude.Operation
	}
	return operation, nil
}

// PendingLifecycleProgress reads the journal phase for presentation. The
// public command remains the recovery authority; this function never mutates
// or attempts to infer a completed transition.
func PendingLifecycleProgress(workspace, slug string) (*LifecycleProgress, error) {
	if !ValidSlug(slug) {
		return nil, errors.New("invalid session slug")
	}
	root := filepath.Join(workspace, "worktrees", ".locks")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect lifecycle journal directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("lifecycle journal directory is unsafe")
	}

	var progress *LifecycleProgress
	for _, journal := range LifecycleJournals() {
		candidate, found, readErr := readLifecycleProgressFile(
			filepath.Join(root, slug+"."+journal.Name+".json"), slug, workspace, journal.Command,
		)
		if readErr != nil {
			return nil, readErr
		}
		if !found {
			continue
		}
		if progress != nil {
			return nil, fmt.Errorf("conflicting lifecycle journals reserve session %s", slug)
		}
		progress = candidate
	}
	prelude, err := readServiceCleanupProgress(workspace, slug)
	if err != nil {
		return nil, err
	}
	return combineServiceCleanup(progress, prelude, slug)
}

func readLifecycleProgressFile(
	path, slug, workspace, operation string,
) (*LifecycleProgress, bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open lifecycle journal progress: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("inspect open lifecycle journal progress: %w", err)
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !opened.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Geteuid()) ||
		opened.Mode().Perm() != 0o600 || opened.Size() > maxLifecycleJournalBytes {
		return nil, false, errors.New("open lifecycle journal progress is unsafe")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLifecycleJournalBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read lifecycle journal progress: %w", err)
	}
	if len(data) > maxLifecycleJournalBytes {
		return nil, false, errors.New("lifecycle journal progress exceeds 64 KiB")
	}
	fields, err := readLifecycleObject(data, maxLifecycleJournalBytes)
	if err != nil {
		return nil, false, err
	}
	// Legacy progress explicitly records a threadless retained root as null.
	// This exception belongs only to progress, not command or deletion input.
	if raw, ok := fields["retained_thread_id"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		delete(fields, "retained_thread_id")
	}
	targets, err := validateLifecycleFields(fields)
	if err != nil {
		return nil, false, err
	}
	var payload struct {
		Slug        string `json:"slug"`
		Workspace   string `json:"workspace"`
		Phase       string `json:"phase"`
		Mode        string `json:"mode"`
		Force       *bool  `json:"force"`
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, false, fmt.Errorf("decode lifecycle journal progress: %w", err)
	}
	if payload.Slug != slug || payload.Workspace != workspace {
		return nil, false, errors.New("lifecycle journal progress has the wrong session identity")
	}
	if _, ok := lifecyclePhases[operation][payload.Phase]; !ok {
		return nil, false, fmt.Errorf("lifecycle journal has an invalid %s phase", operation)
	}
	if operation == "archive" && payload.Mode != "complete" && payload.Mode != "abandoned" && payload.Mode != "archived" {
		return nil, false, errors.New("archive journal progress has an invalid mode")
	}
	if operation == "delete" && payload.Force == nil {
		return nil, false, errors.New("delete journal progress has no force setting")
	}
	if !validLifecycleJournalID(payload.OperationID) {
		return nil, false, errors.New("lifecycle journal has an invalid operation identity")
	}
	force := false
	if payload.Force != nil {
		force = *payload.Force
	}
	// The command owns full journal validation and tracking projections. The
	// portal binds retries to immutable journal bytes, excluding progress and
	// the documented deletion force/inventory updates.
	var immutable map[string]any
	if err := json.Unmarshal(data, &immutable); err != nil {
		return nil, false, err
	}
	if targets != nil {
		if err := targets.Validate(operation == "delete"); err != nil {
			return nil, false, err
		}
		immutable["codex_targets"] = targets
	}
	delete(immutable, "phase")
	if pi, ok := immutable["pi"].(map[string]any); ok {
		encoded, _ := json.Marshal(pi)
		if err := validatePiLifecycleField(encoded, workspace, slug); err != nil {
			return nil, false, err
		}
		delete(pi, "terminal")
	}
	if operation == "delete" {
		for _, key := range []string{"state", "force", "git_tracked", "worktrees", "worktrees_sealed"} {
			delete(immutable, key)
		}
	}
	encoded, err := json.Marshal(immutable)
	if err != nil {
		return nil, false, err
	}
	var retained *string
	root, rootRecorded := immutable["retained_thread_id"]
	if rootRecorded && root != nil {
		value, ok := root.(string)
		if !ok || value == "" {
			return nil, false, errors.New("lifecycle journal has an invalid retained root")
		}
		retained = &value
	}
	return &LifecycleProgress{
		CodexTargets: targets, Operation: operation, Phase: payload.Phase, Mode: payload.Mode,
		Force: force, JournalID: payload.OperationID, UpdatedAt: opened.ModTime(),
		Evidence: fmt.Sprintf("%x", sha256.Sum256(encoded)), RetainedThreadID: retained, RootRecorded: rootRecorded,
	}, true, nil
}

func validLifecycleJournalID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}
