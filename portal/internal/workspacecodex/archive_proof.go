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
	"strings"

	"github.com/aither64/codex-web/codex"
	"golang.org/x/sys/unix"
)

const archiveHeaderLimit = 1024 * 1024

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
	ThreadID  string
	Cwd       string
	ProjectID string
	CodexHome string
}

type ThreadMetadataReader interface {
	ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
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
		if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() || err != nil && !errors.Is(err, os.ErrNotExist) {
			return ArchiveUnknown, errors.New("active rollout has an invalid file identity")
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
	if c.archiveProof != nil {
		return c.archiveProof(ctx, threadID, cwd, projectID)
	}
	return ProveArchivedThread(ctx, c.Client, ArchivedThreadIdentity{
		ThreadID: threadID, Cwd: cwd, ProjectID: projectID, CodexHome: c.CodexHome,
	})
}

func validateArchiveMetadata(metadata codex.ThreadMetadata, expected ArchivedThreadIdentity) error {
	if metadata.ID != expected.ThreadID || metadata.Cwd != expected.Cwd {
		return errors.New("thread/read returned the wrong retained identity")
	}
	if expected.ProjectID != "" && (metadata.ProjectID == nil || *metadata.ProjectID != expected.ProjectID) {
		return errors.New("thread/read returned the wrong retained project")
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

func readArchiveHeader(file *os.File) (struct {
	ID  string  `json:"id"`
	Cwd *string `json:"cwd"`
}, error) {
	var payload struct {
		ID  string  `json:"id"`
		Cwd *string `json:"cwd"`
	}
	line, err := bufio.NewReader(io.LimitReader(file, archiveHeaderLimit+1)).ReadBytes('\n')
	if err != nil {
		return payload, errors.New("archived rollout has no complete first metadata record")
	}
	if len(line) > archiveHeaderLimit || len(line) == 0 || line[len(line)-1] != '\n' {
		return payload, errors.New("archived rollout metadata record exceeds 1 MiB")
	}
	record, err := uniqueArchiveObject(bytes.TrimSuffix(line, []byte{'\n'}))
	var recordType string
	if err != nil || json.Unmarshal(record["type"], &recordType) != nil || recordType != "session_meta" {
		return payload, errors.New("archived rollout has an invalid session_meta record")
	}
	fields, err := uniqueArchiveObject(record["payload"])
	if err != nil || json.Unmarshal(fields["id"], &payload.ID) != nil || payload.ID == "" {
		return payload, errors.New("archived rollout has an invalid session_meta identity")
	}
	if cwd, present := fields["cwd"]; present {
		var value string
		if json.Unmarshal(cwd, &value) != nil {
			return payload, errors.New("archived rollout has an invalid session_meta directory")
		}
		payload.Cwd = &value
	}
	return payload, nil
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
