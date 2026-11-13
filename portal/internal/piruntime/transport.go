package piruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type controlRequest struct {
	Schema    int             `json:"schema"`
	ID        string          `json:"id"`
	Runtime   RuntimeRef      `json:"runtime"`
	Operation string          `json:"operation"`
	Body      json.RawMessage `json:"body"`
}
type controlResponse struct {
	Schema  int             `json:"schema"`
	ID      string          `json:"id"`
	Runtime RuntimeRef      `json:"runtime"`
	Body    json.RawMessage `json:"body,omitempty"`
	Error   string          `json:"error,omitempty"`
}

func peerUID(connection net.Conn) (uint32, error) {
	unix, ok := connection.(*net.UnixConn)
	if !ok {
		return 0, ErrScope
	}
	raw, err := unix.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credentials *syscall.Ucred
	var controlErr error
	err = raw.Control(func(fd uintptr) {
		credentials, controlErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if controlErr != nil {
		return 0, controlErr
	}
	return credentials.Uid, nil
}

func (m *Manager) callRaw(ctx context.Context, ref RuntimeRef, operation string, body, result any) error {
	// /proc/self/fd keeps the fixed filesystem socket usable even when the
	// configured instance path exceeds Linux's sockaddr_un pathname limit.
	directory, err := os.Open(m.instance(ref))
	if err != nil {
		return err
	}
	defer directory.Close()
	path := filepath.Join("/proc/self/fd", strconv.Itoa(int(directory.Fd())), "control.sock")
	connection, err := m.config.Dial(ctx, path)
	if err != nil {
		return err
	}
	defer connection.Close()
	uid, err := peerUID(connection)
	if err != nil || uid != uint32(os.Geteuid()) {
		return ErrScope
	}
	deadline := time.Now().Add(30 * time.Second)
	if earlier, ok := ctx.Deadline(); ok && earlier.Before(deadline) {
		deadline = earlier
	}
	_ = connection.SetDeadline(deadline)
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request := controlRequest{Schema: 1, ID: newUUID(), Runtime: ref, Operation: operation, Body: encoded}
	data, err := json.Marshal(request)
	if err != nil || len(data) > recordLimit {
		return ErrScope
	}
	data = append(data, '\n')
	if _, err := connection.Write(data); err != nil {
		return err
	}
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 4096), recordLimit)
	if !scanner.Scan() {
		return fmt.Errorf("Pi supervisor response unavailable: %w", scanner.Err())
	}
	var response controlResponse
	if err := strictDecode(scanner.Bytes(), &response, recordLimit); err != nil {
		return err
	}
	if response.Schema != 1 || response.ID != request.ID || response.Runtime != ref {
		return ErrScope
	}
	if result != nil && len(response.Body) != 0 {
		if err := strictDecode(response.Body, result, recordLimit); err != nil {
			return err
		}
	}
	switch response.Error {
	case "":
	case "scope":
		return ErrScope
	case "held":
		return ErrHeld
	case "conflict":
		return ErrConflict
	default:
		return ErrUnavailable
	}
	return nil
}
