package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

const serviceCleanupSuffix = ".services-cleanup.json"

var serviceCleanupUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// This subordinate prelude owns admission only until the ordinary journal has
// been published. It is not another lifecycle command or journal format.
func readServiceCleanupProgress(workspace, slug string) (*LifecycleProgress, error) {
	path := filepath.Join(workspace, "worktrees", ".locks", slug+serviceCleanupSuffix)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0600 || info.Size() > maxLifecycleJournalBytes {
		return nil, errors.New("service cleanup prelude is unsafe")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxLifecycleJournalBytes+1))
	if err != nil || len(raw) > maxLifecycleJournalBytes {
		return nil, errors.New("service cleanup prelude cannot be read")
	}
	var value struct {
		Schema           int     `json:"schema"`
		Workspace        string  `json:"workspace"`
		Slug             string  `json:"slug"`
		RegistryID       string  `json:"registry_id"`
		RetainedThreadID *string `json:"retained_thread_id"`
		TrackingIdentity struct {
			Dev *uint64 `json:"dev"`
			Ino *uint64 `json:"ino"`
		} `json:"tracking_identity"`
		Operation   string   `json:"operation"`
		OperationID string   `json:"operation_id"`
		Mode        *string  `json:"mode"`
		Force       *bool    `json:"force"`
		Automatic   *bool    `json:"automatic"`
		ResourceIDs []string `json:"resource_ids"`
		Phase       string   `json:"phase"`
		CreatedAt   string   `json:"created_at"`
		UpdatedAt   string   `json:"updated_at"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode service cleanup prelude: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("service cleanup prelude has trailing data")
	}
	var tree yaml.Node
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	if err := rejectAliasesAndDuplicateKeys(&tree); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if len(fields) != 15 || len(fields["retained_thread_id"]) == 0 || len(fields["mode"]) == 0 || len(fields["force"]) == 0 || value.Schema != 1 || value.Workspace != workspace || value.Slug != slug || !serviceCleanupUUID.MatchString(value.RegistryID) || !validLifecycleJournalID(value.OperationID) || value.Automatic == nil || value.TrackingIdentity.Dev == nil || value.TrackingIdentity.Ino == nil || value.RetainedThreadID != nil && *value.RetainedThreadID == "" || value.ResourceIDs == nil || len(value.ResourceIDs) > 256 || value.Phase != "services_pending" && value.Phase != "services_stopped" {
		return nil, errors.New("service cleanup prelude has invalid ownership or state")
	}
	mode := ""
	force := false
	switch value.Operation {
	case "archive":
		if value.Mode == nil || value.Force != nil {
			return nil, errors.New("service cleanup archive policy is invalid")
		}
		mode = *value.Mode
		if mode != "complete" && mode != "abandoned" && mode != "archived" {
			return nil, errors.New("service cleanup archive mode is invalid")
		}
	case "delete":
		if value.Force == nil || value.Mode != nil {
			return nil, errors.New("service cleanup delete policy is invalid")
		}
		force = *value.Force
	default:
		return nil, errors.New("service cleanup operation is invalid")
	}
	if !sort.StringsAreSorted(value.ResourceIDs) {
		return nil, errors.New("service cleanup resource identities are invalid")
	}
	for index, id := range value.ResourceIDs {
		if !serviceCleanupUUID.MatchString(id) || index > 0 && id == value.ResourceIDs[index-1] {
			return nil, errors.New("service cleanup resource identities are invalid")
		}
	}
	for _, value := range []string{value.CreatedAt, value.UpdatedAt} {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, errors.New("service cleanup time is invalid")
		}
		_, offset := parsed.Zone()
		if offset != 0 {
			return nil, errors.New("service cleanup time is not UTC")
		}
	}
	var immutable map[string]any
	if err := json.Unmarshal(raw, &immutable); err != nil {
		return nil, err
	}
	delete(immutable, "phase")
	delete(immutable, "updated_at")
	if value.Operation == "delete" {
		delete(immutable, "force")
	}
	encoded, err := json.Marshal(immutable)
	if err != nil {
		return nil, err
	}
	return &LifecycleProgress{Operation: value.Operation, JournalID: value.OperationID, Mode: mode, Force: force, Phase: value.Phase, UpdatedAt: info.ModTime(), RetainedThreadID: value.RetainedThreadID, RootRecorded: true, Evidence: fmt.Sprintf("%x", sha256.Sum256(encoded)), ServicePrelude: true}, nil
}

func combineServiceCleanup(progress, prelude *LifecycleProgress, slug string) (*LifecycleProgress, error) {
	if prelude == nil {
		return progress, nil
	}
	if progress == nil {
		return prelude, nil
	}
	sameRoot := !progress.RootRecorded || (progress.RetainedThreadID == nil && prelude.RetainedThreadID == nil || progress.RetainedThreadID != nil && prelude.RetainedThreadID != nil && *progress.RetainedThreadID == *prelude.RetainedThreadID)
	if progress.Operation != prelude.Operation || progress.JournalID != prelude.JournalID || progress.Mode != prelude.Mode || !sameRoot || progress.Operation == "delete" && prelude.Force && !progress.Force {
		return nil, fmt.Errorf("conflicting service cleanup and lifecycle journals reserve session %s", slug)
	}
	// Ordinary evidence is authoritative, including delete from its first phase.
	progress.ServicePreludeEvidence = prelude.Evidence
	return progress, nil
}
