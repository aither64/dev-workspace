package workspacecodex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/aither64/codex-web/codex"
	"golang.org/x/sys/unix"
)

const archiveHeaderLimit = 1024 * 1024

// ArchiveDiscoverySourceKinds is the complete selected 0.160.0 generated
// ThreadSourceKind enum. Omitted/empty filters enumerate interactive sources
// only. Keep this archive-only set aligned with the selected protocol; ordinary
// RetireThread discovery deliberately retains its existing source policy.
func ArchiveDiscoverySourceKinds() []string {
	return []string{
		"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview",
		"subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown",
	}
}

var archiveThreadIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var archiveRolloutPattern = regexp.MustCompile(`^rollout-[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

type ArchiveState uint8

const (
	ArchiveUnknown ArchiveState = iota
	ArchiveFresh
	ArchiveActive
	ArchiveArchived
)

type ArchivedThreadIdentity struct {
	ThreadID          string
	Cwd               string
	ProjectID         string
	CodexHome         string
	SourceKind        string
	RequireActiveFile bool
}

type ThreadMetadataReader interface {
	ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
}

// Bind the existing rollout proof to the metadata/status sampled for idle
// checks, so its independent reads cannot prove a different path or project.
type archiveIdleMetadataReader struct {
	ThreadMetadataReader
	metadata codex.ThreadMetadata
}

func (reader archiveIdleMetadataReader) ReadThreadMetadata(ctx context.Context, threadID string, excludeTurns bool) (codex.ThreadMetadata, error) {
	metadata, err := reader.ThreadMetadataReader.ReadThreadMetadata(ctx, threadID, excludeTurns)
	if err != nil {
		return codex.ThreadMetadata{}, err
	}
	if !reflect.DeepEqual(metadata, reader.metadata) {
		return codex.ThreadMetadata{}, errors.New("archived conversation metadata changed during idle proof")
	}
	return metadata, nil
}

func ProveArchivedThread(ctx context.Context, reader ThreadMetadataReader, expected ArchivedThreadIdentity) (ArchiveState, error) {
	if reader == nil || !archiveThreadIDPattern.MatchString(expected.ThreadID) ||
		!canonicalArchivePath(expected.Cwd) || !canonicalArchivePath(expected.CodexHome) {
		return ArchiveUnknown, errors.New("archive proof requires a retained thread, canonical directory and Codex home")
	}
	resolvedHome, err := filepath.EvalSymlinks(expected.CodexHome)
	if err != nil || resolvedHome != expected.CodexHome {
		return ArchiveUnknown, errors.New("archive proof Codex home is not canonical")
	}
	metadata, err := reader.ReadThreadMetadata(ctx, expected.ThreadID, false)
	if err != nil {
		return ArchiveUnknown, fmt.Errorf("read exact Codex thread metadata: %w", err)
	}
	if err := validateArchiveMetadata(metadata, expected); err != nil {
		return ArchiveUnknown, err
	}
	if metadata.Path == nil {
		return ArchiveFresh, nil
	}
	path := *metadata.Path
	if !canonicalArchivePath(path) {
		return ArchiveUnknown, errors.New("Codex thread has an invalid rollout path")
	}
	match := archiveRolloutPattern.FindStringSubmatch(filepath.Base(path))
	if len(match) != 2 || match[1] != expected.ThreadID {
		return ArchiveUnknown, errors.New("rollout filename does not match the retained thread")
	}
	if archivePathWithin(filepath.Join(expected.CodexHome, "sessions"), path) {
		resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil || resolvedParent != filepath.Dir(path) {
			return ArchiveUnknown, errors.New("active rollout directory is not canonical")
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && expected.RequireActiveFile {
			return ArchiveUnknown, errors.New("active rollout is not materialized")
		}
		if err == nil && !info.Mode().IsRegular() || err != nil && !errors.Is(err, os.ErrNotExist) {
			return ArchiveUnknown, errors.New("active rollout has an invalid file identity")
		}
		if expected.RequireActiveFile {
			again, readErr := reader.ReadThreadMetadata(ctx, expected.ThreadID, false)
			current, statErr := os.Lstat(path)
			if readErr != nil || !reflect.DeepEqual(metadata, again) || statErr != nil || !os.SameFile(info, current) {
				return ArchiveUnknown, errors.New("active rollout changed during materialization proof")
			}
		}
		return ArchiveActive, nil
	}
	if !archivePathWithin(filepath.Join(expected.CodexHome, "archived_sessions"), path) {
		return ArchiveUnknown, errors.New("Codex thread path is outside this authority's archive")
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || resolvedParent != filepath.Dir(path) {
		return ArchiveUnknown, errors.New("archived rollout directory is not canonical")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return ArchiveUnknown, fmt.Errorf("inspect archived rollout: %w", err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return ArchiveUnknown, errors.New("archived rollout is not a regular non-symlink file")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return ArchiveUnknown, fmt.Errorf("open archived rollout: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return ArchiveUnknown, errors.New("archived rollout changed while opening")
	}
	first, err := readArchiveHeader(file)
	if err != nil {
		return ArchiveUnknown, err
	}
	if first.ID != expected.ThreadID || first.Cwd != nil && *first.Cwd != expected.Cwd {
		return ArchiveUnknown, errors.New("archived rollout header has the wrong thread identity")
	}
	if err := ctx.Err(); err != nil {
		return ArchiveUnknown, err
	}
	end, err := file.Stat()
	if err != nil || !os.SameFile(before, end) || end.Size() != before.Size() || !end.ModTime().Equal(before.ModTime()) {
		return ArchiveUnknown, errors.New("archived rollout changed during proof")
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, current) || current.Size() != before.Size() || !current.ModTime().Equal(before.ModTime()) {
		return ArchiveUnknown, errors.New("archived rollout was replaced during proof")
	}
	again, err := reader.ReadThreadMetadata(ctx, expected.ThreadID, false)
	if err != nil || !reflect.DeepEqual(metadata, again) {
		return ArchiveUnknown, errors.New("Codex thread metadata changed during archive proof")
	}
	current, err = os.Lstat(path)
	if err != nil || !os.SameFile(before, current) || current.Size() != before.Size() || !current.ModTime().Equal(before.ModTime()) {
		return ArchiveUnknown, errors.New("archived rollout changed during metadata recheck")
	}
	return ArchiveArchived, nil
}

func (c *Client) ProveArchivedThread(ctx context.Context, threadID, cwd, projectID string) (ArchiveState, error) {
	return c.proveArchivedThread(ctx, threadID, cwd, projectID, "", false)
}

func (c *Client) ProveArchivedRootThread(ctx context.Context, threadID, cwd string) (ArchiveState, error) {
	return c.proveArchivedThread(ctx, threadID, cwd, "", threadSourceKind, false)
}

// RequireRootArchiveReady is a read-only preflight. Archived roots retain
// their exact rollout proof; active and fresh roots also need ordinary idle
// proof. It does not relax RetireThread's independent directory discovery.
func (c *Client) RequireRootArchiveReady(ctx context.Context, threadID, cwd string) (ArchiveState, error) {
	state, err := c.ProveArchivedRootThread(ctx, threadID, cwd)
	if err != nil {
		return ArchiveUnknown, err
	}
	if err := c.RequireArchiveThreadIdle(ctx, threadID, cwd, state); err != nil {
		return ArchiveUnknown, err
	}
	again, err := c.ProveArchivedRootThread(ctx, threadID, cwd)
	if err != nil {
		return ArchiveUnknown, fmt.Errorf("recheck retained root during archive preflight: %w", err)
	}
	if again != state {
		return ArchiveUnknown, errors.New("retained root archive state changed during preflight")
	}
	return state, nil
}

// Positive archived/unloaded proof excludes currently runnable input. Native
// archive can retain dormant queue rows, and queue/list rejects a cold archive.
// A later explicit resume can make those rows runnable; this proof preserves them.
func (c *Client) RequireArchiveThreadIdle(ctx context.Context, threadID, cwd string, state ArchiveState) error {
	if state == ArchiveActive || state == ArchiveFresh {
		return c.RequireThreadIdle(ctx, threadID, cwd)
	}
	if state != ArchiveArchived {
		return errors.New("retained conversation has no proved archive state")
	}
	metadata, before, err := c.requireArchivedThreadUnloaded(ctx, threadID, cwd)
	if err != nil {
		return err
	}
	if err := c.RequireThreadTurnsIdle(ctx, threadID); err != nil {
		return fmt.Errorf("inspect archived conversation turns: %w", err)
	}
	prompts, err := c.PromptsWithItems(ctx, threadID)
	if err != nil {
		return fmt.Errorf("inspect archived conversation requests: %w", err)
	}
	if len(prompts) != 0 {
		return errors.New("archived conversation has pending requests")
	}
	if err := c.RequireSubmissionAttemptsResolved(ctx, threadID); err != nil {
		return observationFailure("submission_unverified", err.Error())
	}
	if err := c.RequireThreadTurnsIdle(ctx, threadID); err != nil {
		return fmt.Errorf("recheck archived conversation turns: %w", err)
	}
	again, after, err := c.requireArchivedThreadUnloaded(ctx, threadID, cwd)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(metadata, again) || !os.SameFile(before, after) ||
		before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("archived conversation changed during idle proof")
	}
	return nil
}

// LoadedThreadIDs omits internal native sources. Its absence proof is valid
// here only for an exactly retained vscode thread with a proved archived file.
func (c *Client) requireArchivedThreadUnloaded(ctx context.Context, threadID, cwd string) (codex.ThreadMetadata, os.FileInfo, error) {
	metadata, err := c.ReadThreadMetadata(ctx, threadID, false)
	if err != nil {
		return codex.ThreadMetadata{}, nil, fmt.Errorf("read archived conversation status: %w", err)
	}
	if err := validateArchiveMetadata(metadata, ArchivedThreadIdentity{
		ThreadID: threadID, Cwd: cwd, SourceKind: threadSourceKind,
	}); err != nil {
		return codex.ThreadMetadata{}, nil, err
	}
	if len(metadata.Status) != 1 || metadata.Status["type"] != "notLoaded" || metadata.Path == nil ||
		!canonicalArchivePath(*metadata.Path) || !archivePathWithin(filepath.Join(c.CodexHome, "archived_sessions"), *metadata.Path) {
		return codex.ThreadMetadata{}, nil, errors.New("archived conversation is not positively unloaded")
	}
	file, err := os.Lstat(*metadata.Path)
	if err != nil || !file.Mode().IsRegular() {
		return codex.ThreadMetadata{}, nil, errors.New("archived conversation file changed during idle proof")
	}
	state, err := ProveArchivedThread(ctx, archiveIdleMetadataReader{c.Client, metadata}, ArchivedThreadIdentity{
		ThreadID: threadID, Cwd: cwd, CodexHome: c.CodexHome, SourceKind: threadSourceKind,
	})
	if err != nil {
		return codex.ThreadMetadata{}, nil, fmt.Errorf("reprove archived conversation identity: %w", err)
	}
	if state != ArchiveArchived {
		return codex.ThreadMetadata{}, nil, errors.New("retained conversation is no longer positively archived")
	}
	loaded, err := c.LoadedThreadIDs(ctx)
	if err != nil {
		return codex.ThreadMetadata{}, nil, fmt.Errorf("inspect loaded archived conversation: %w", err)
	}
	if slices.Contains(loaded, threadID) {
		return codex.ThreadMetadata{}, nil, errors.New("archived conversation is still loaded")
	}
	return metadata, file, nil
}

func (c *Client) ProveMaterializedActiveThread(ctx context.Context, threadID, cwd string) (ArchiveState, error) {
	return c.proveArchivedThread(ctx, threadID, cwd, "", "", true)
}

func (c *Client) proveArchivedThread(ctx context.Context, threadID, cwd, projectID, sourceKind string, requireActiveFile bool) (ArchiveState, error) {
	if c.archiveProof != nil {
		return c.archiveProof(ctx, threadID, cwd, projectID)
	}
	return ProveArchivedThread(ctx, c.Client, ArchivedThreadIdentity{
		ThreadID: threadID, Cwd: cwd, ProjectID: projectID, CodexHome: c.CodexHome, SourceKind: sourceKind,
		RequireActiveFile: requireActiveFile,
	})
}

func validateArchiveMetadata(metadata codex.ThreadMetadata, expected ArchivedThreadIdentity) error {
	if metadata.ID != expected.ThreadID || metadata.Cwd != expected.Cwd {
		return errors.New("thread/read returned the wrong retained identity")
	}
	if expected.ProjectID != "" && (metadata.ProjectID == nil || *metadata.ProjectID != expected.ProjectID) {
		return errors.New("thread/read returned the wrong retained project")
	}
	if expected.SourceKind != "" {
		source, ok := metadata.Source.(string)
		if !ok || source != expected.SourceKind {
			return errors.New("thread/read returned the wrong retained source")
		}
	}
	return nil
}

func canonicalArchivePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

func archivePathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

type archiveHeader struct {
	ID  string  `json:"id"`
	Cwd *string `json:"cwd"`
}

func readArchiveHeader(file *os.File) (archiveHeader, error) {
	payload, _, err := readArchiveHeaderRecord(file)
	return payload, err
}

// Preserve the exact bounded prefix for scope rechecks without reading turns.
func readArchiveHeaderRecord(reader io.Reader) (archiveHeader, []byte, error) {
	var payload archiveHeader
	line, err := bufio.NewReader(io.LimitReader(reader, archiveHeaderLimit+1)).ReadBytes('\n')
	if err != nil {
		return payload, line, errors.New("archived rollout has no complete first metadata record")
	}
	if len(line) > archiveHeaderLimit || len(line) == 0 || line[len(line)-1] != '\n' {
		return payload, line, errors.New("archived rollout metadata record exceeds 1 MiB")
	}
	record, err := uniqueArchiveObject(bytes.TrimSuffix(line, []byte{'\n'}))
	var recordType string
	if err != nil || json.Unmarshal(record["type"], &recordType) != nil || recordType != "session_meta" {
		return payload, line, errors.New("archived rollout has an invalid session_meta record")
	}
	fields, err := uniqueArchiveObject(record["payload"])
	if err != nil || json.Unmarshal(fields["id"], &payload.ID) != nil || payload.ID == "" {
		return payload, line, errors.New("archived rollout has an invalid session_meta identity")
	}
	if cwd, present := fields["cwd"]; present {
		var value string
		if json.Unmarshal(cwd, &value) != nil {
			return payload, line, errors.New("archived rollout has an invalid session_meta directory")
		}
		payload.Cwd = &value
	}
	return payload, line, nil
}

func uniqueArchiveObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("archive metadata is not a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			return nil, errors.New("archive metadata has an invalid key")
		}
		if _, exists := fields[name]; exists {
			return nil, errors.New("archive metadata has duplicate keys")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, errors.New("archive metadata has no closing object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("archive metadata has trailing data")
	}
	return fields, nil
}
