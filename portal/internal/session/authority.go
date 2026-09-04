package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

const authorityMaxSize = 64 * 1024

type RuntimeAuthority struct {
	Schema             int    `json:"schema"`
	State              string `json:"state"`
	Slug               string `json:"slug"`
	Workspace          string `json:"workspace"`
	TmuxSocket         string `json:"tmux_socket"`
	TmuxSessionID      string `json:"tmux_session_id"`
	CodexThreadID      string `json:"codex_thread_id,omitempty"`
	CodexSocketPath    string `json:"codex_socket_path,omitempty"`
	CodexClientVersion string `json:"codex_client_version,omitempty"`
}

type RuntimeLock struct {
	session *os.File
}

func (l *RuntimeLock) Close() error {
	if l.session == nil {
		return nil
	}
	return l.session.Close()
}

// LockRuntimeShared serializes browser mutations with lifecycle commands for
// one session. The lock lives outside the LXC-writable workspace.
func LockRuntimeShared(directory, slug string) (*RuntimeLock, error) {
	if directory == "" || !ValidSlug(slug) {
		return nil, errors.New("runtime locking is not configured")
	}
	directoryFD, err := openAuthorityDirectory(directory)
	if err != nil {
		return nil, err
	}
	defer unix.Close(directoryFD)

	session, err := openOwnedLock(directoryFD, slug+".lock")
	if err != nil {
		return nil, fmt.Errorf("open runtime session lock: %w", err)
	}
	if err := unix.Flock(int(session.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		session.Close()
		return nil, fmt.Errorf("session is changing: %w", err)
	}
	return &RuntimeLock{session: session}, nil
}

func openAuthorityDirectory(directory string) (int, error) {
	directoryFD, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open runtime authority directory: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(directoryFD, &stat); err != nil {
		unix.Close(directoryFD)
		return -1, fmt.Errorf("inspect runtime authority directory: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o777 != 0o700 {
		unix.Close(directoryFD)
		return -1, errors.New("runtime authority directory must be owned by the portal uid with mode 0700")
	}
	return directoryFD, nil
}

func openOwnedLock(directoryFD int, path string) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_CREAT
	fd, err := unix.Openat(directoryFD, path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		unix.Close(fd)
		return nil, errors.New("invalid lock file descriptor")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o777 != 0o600 {
		file.Close()
		return nil, errors.New("runtime lock must be a portal-uid mode-0600 regular file")
	}
	return file, nil
}

func LoadRuntimeAuthority(directory, slug, workspace string) (RuntimeAuthority, error) {
	var authority RuntimeAuthority
	if directory == "" || !ValidSlug(slug) {
		return authority, errors.New("runtime authority is not configured")
	}
	directoryFD, err := openAuthorityDirectory(directory)
	if err != nil {
		return authority, err
	}
	defer unix.Close(directoryFD)
	fileFD, err := unix.Openat(directoryFD, slug+".json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return authority, fmt.Errorf("open runtime authority: %w", err)
	}
	file := os.NewFile(uintptr(fileFD), slug+".json")
	if file == nil {
		unix.Close(fileFD)
		return authority, errors.New("open runtime authority: invalid file descriptor")
	}
	defer file.Close()
	var fileStat unix.Stat_t
	if err := unix.Fstat(fileFD, &fileStat); err != nil {
		return authority, fmt.Errorf("inspect runtime authority: %w", err)
	}
	if fileStat.Mode&unix.S_IFMT != unix.S_IFREG || fileStat.Uid != uint32(os.Geteuid()) || fileStat.Mode&0o777 != 0o600 {
		return authority, errors.New("runtime authority must be a portal-uid mode-0600 regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, authorityMaxSize+1))
	if err != nil {
		return authority, fmt.Errorf("read runtime authority: %w", err)
	}
	if len(data) > authorityMaxSize {
		return authority, errors.New("runtime authority exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&authority); err != nil {
		return RuntimeAuthority{}, fmt.Errorf("decode runtime authority: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return RuntimeAuthority{}, errors.New("runtime authority contains trailing JSON data")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return RuntimeAuthority{}, fmt.Errorf("decode runtime authority fields: %w", err)
	}
	codexKeys := []string{"codex_thread_id", "codex_socket_path", "codex_client_version"}
	present := 0
	for _, key := range codexKeys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		present++
		var text string
		if err := json.Unmarshal(value, &text); err != nil || text == "" {
			return RuntimeAuthority{}, errors.New("invalid runtime Codex authority")
		}
	}
	if present != 0 && present != len(codexKeys) {
		return RuntimeAuthority{}, errors.New("invalid runtime Codex authority")
	}
	if err := authority.Validate(slug, workspace); err != nil {
		return RuntimeAuthority{}, err
	}
	return authority, nil
}

func (a RuntimeAuthority) Validate(slug, workspace string) error {
	if a.Schema != 1 || (a.State != "creating" && a.State != "ready") ||
		a.Slug != slug || a.Workspace != workspace ||
		!validSocketPath(a.TmuxSocket) || len(a.TmuxSessionID) < 2 ||
		a.TmuxSessionID[0] != '$' || strings.Trim(a.TmuxSessionID[1:], "0123456789") != "" {
		return errors.New("invalid runtime authority identity")
	}
	codexCount := 0
	for _, value := range []string{a.CodexThreadID, a.CodexSocketPath, a.CodexClientVersion} {
		if value != "" {
			codexCount++
		}
	}
	if codexCount != 0 && (codexCount != 3 || !validSocketPath(a.CodexSocketPath) ||
		!clientVersionPattern.MatchString(a.CodexClientVersion)) {
		return errors.New("invalid runtime Codex authority")
	}
	return nil
}

func (a RuntimeAuthority) VerifyTmux(ctx context.Context, tmux string) error {
	if tmux == "" {
		tmux = "tmux"
	}
	format := strings.Join([]string{
		"#{session_id}", "#{session_name}", "#{@vpsfree_dev_session}",
		"#{@vpsfree_dev_session_slug}", "#{E:VPSFREE_DEV_SESSION_WORKSPACE}",
		"#{E:VPSFREE_DEV_SESSION_SLUG}", "#{socket_path}",
		"#{@vpsfree_dev_session_codex_thread}",
		"#{@vpsfree_dev_session_codex_socket}",
		"#{@vpsfree_dev_session_codex_version}",
		"#{@vpsfree_dev_session_codex_pane}",
	}, "\t")
	command := exec.CommandContext(
		ctx, tmux, "-S", a.TmuxSocket, "display-message", "-p",
		"-t", a.TmuxSessionID+":", format,
	)
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("inspect live tmux session: %w", err)
	}
	fields := strings.Split(strings.TrimSuffix(string(output), "\n"), "\t")
	if len(fields) != 11 || fields[0] != a.TmuxSessionID || fields[1] != a.Slug ||
		fields[2] != "1" || fields[3] != a.Slug || fields[4] != a.Workspace ||
		fields[5] != a.Slug || fields[6] != a.TmuxSocket ||
		fields[7] != a.CodexThreadID || fields[8] != a.CodexSocketPath ||
		fields[9] != a.CodexClientVersion ||
		(a.CodexThreadID != "" && (len(fields[10]) < 2 || fields[10][0] != '%' || strings.Trim(fields[10][1:], "0123456789") != "")) {
		return errors.New("live tmux session does not match runtime authority")
	}
	return nil
}
