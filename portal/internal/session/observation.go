package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// ObservationIdentity binds a cached semantic baseline to the retained root
// (including explicit absence) and directory, independent of metadata writes.
// Unlike browser confirmations it survives an accepted tracking-directory move.
func ObservationIdentity(workspace, slug, directory, rootThreadID string) (string, error) {
	info, err := os.Lstat(directory)
	resolved, resolveErr := filepath.EvalSymlinks(directory)
	if err != nil || resolveErr != nil || !info.IsDir() || resolved != directory {
		return "", errors.New("tracking observation identity cannot be verified")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("tracking observation has no filesystem identity")
	}
	var root any
	if rootThreadID != "" {
		root = rootThreadID
	}
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode([]any{"session-observation-v1", workspace, slug, stat.Dev, stat.Ino, root}); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(payload.Bytes(), []byte{'\n'}))
	return hex.EncodeToString(sum[:]), nil
}

// RequireNormalizedThreadless requires explicit scope, not a missing root
// field in a partial/legacy manifest. The normal manifest owner still validates
// all repository and artifact content before this additional presence check.
func RequireNormalizedThreadless(summary *Summary) error {
	if summary == nil || summary.Schema != 1 || summary.Codex.ThreadID != "" {
		return errors.New("normalized threadless tracking is required")
	}
	file, err := openConfined(summary.Workspace, filepath.Join(summary.Root, summary.Slug, ManifestName), unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, manifestMaxSize+1))
	if err != nil || len(data) > manifestMaxSize {
		return errors.New("threadless manifest cannot be read")
	}
	var manifest Manifest
	if err := decodeManifest(data, &manifest); err != nil {
		return err
	}
	if err := manifest.Validate(summary.Slug); err != nil {
		return err
	}
	var fields map[string]yaml.Node
	if err := yaml.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"repositories", "artifacts"} {
		value, present := fields[name]
		if !present || value.Kind != yaml.SequenceNode {
			return errors.New("threadless scope lists must be explicit")
		}
	}
	for _, name := range []string{"codex", "creation", "forked_from"} {
		if _, present := fields[name]; present {
			return errors.New("conversation evidence contradicts threadless tracking")
		}
	}
	return nil
}

// RequireNoRuntime samples only the host-selected authority and tmux socket.
// Missing state is checked as filesystem absence; command/RPC errors never
// stand for an empty server. No authority, client or pane is changed.
func RequireNoRuntime(ctx context.Context, workspace, slug, authorityDir, tmuxSocket string) error {
	if !ValidSlug(slug) || !validSocketPath(tmuxSocket) {
		return errors.New("selected threadless runtime coordinates are unavailable")
	}
	if fd, err := openAuthorityDirectory(authorityDir); err == nil {
		unix.Close(fd)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(authorityDir, slug+".json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("runtime authority contradicts threadless tracking")
	}
	if _, err := os.Lstat(tmuxSocket); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, "tmux", "-S", tmuxSocket, "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return errors.New("selected tmux absence cannot be verified")
	}
	if len(output) > 64*1024 {
		return errors.New("selected tmux inventory is too large")
	}
	for _, name := range strings.Split(strings.TrimSuffix(string(output), "\n"), "\n") {
		if name == slug {
			return errors.New("live tmux contradicts threadless tracking")
		}
	}
	return nil
}
