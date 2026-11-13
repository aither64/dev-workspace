package piruntime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
)

const callbackLimit = 65536

var shippedHostTools = []string{"monitor_operation", "publish_plan", "report_to_lead", "request_user_input_async", "team_message"}

type nativeHostCall struct {
	InputOperationID string          `json:"inputOperationID"`
	AnchorEntryID    string          `json:"anchorEntryID"`
	NativeToolCallID string          `json:"nativeToolCallID"`
	SelectionToken   string          `json:"selectionToken"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	ArgumentsSHA256  string          `json:"argumentsSHA256"`
}
type hostFrame struct {
	Schema       int        `json:"schema"`
	AttachmentID string     `json:"attachmentID"`
	Runtime      RuntimeRef `json:"runtime"`
	Event        string     `json:"event"`
	ID           string     `json:"id"`
	Call         *HostCall  `json:"call,omitempty"`
	Reply        *HostReply `json:"reply,omitempty"`
}
type hostAttachment struct {
	id         string
	tools      []string
	connection net.Conn
	writeMu    sync.Mutex
	pending    map[string]chan HostReply
	closed     chan struct{}
}

func marshalWire(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	if buffer.Len() > recordLimit {
		return nil, ErrScope
	}
	return buffer.Bytes(), nil
}
func hostToolsValid(names []string) bool {
	if len(names) > len(shippedHostTools) || !slices.IsSorted(names) {
		return false
	}
	for i, name := range names {
		if !slices.Contains(shippedHostTools, name) || (i > 0 && names[i-1] == name) {
			return false
		}
	}
	return true
}
func validHostReply(reply HostReply) bool {
	if len(reply.Body) > callbackLimit {
		return false
	}
	switch reply.Outcome {
	case "result":
		return len(reply.Body) > 0 && !bytes.Equal(bytes.TrimSpace(reply.Body), []byte("null")) && json.Valid(reply.Body)
	case "refused", "unknown":
		return slices.Contains([]string{"unavailable", "invalid", "scope", "stale", "conflict", "canceled", "unknown"}, reply.Code)
	default:
		return false
	}
}

func (s *supervisor) attachHost(connection *net.UnixConn, request controlRequest) {
	var body struct {
		Tools []string `json:"tools"`
	}
	response := controlResponse{Schema: 1, ID: request.ID, Runtime: s.ref}
	if decodeBody(request.Body, &body) != nil || !hostToolsValid(body.Tools) {
		response.Error = "scope"
		data, _ := marshalWire(response)
		_, _ = connection.Write(data)
		return
	}
	s.callbackMu.Lock()
	if s.attachment != nil {
		s.callbackMu.Unlock()
		response.Error = "held"
		data, _ := marshalWire(response)
		_, _ = connection.Write(data)
		return
	}
	attachment := &hostAttachment{id: newUUID(), tools: append([]string{}, body.Tools...), connection: connection, pending: map[string]chan HostReply{}, closed: make(chan struct{})}
	s.attachment = attachment
	s.callbackMu.Unlock()
	defer func() {
		s.callbackMu.Lock()
		if s.attachment == attachment {
			s.attachment = nil
		}
		close(attachment.closed)
		s.callbackMu.Unlock()
	}()
	response.Body, _ = json.Marshal(struct {
		ID string `json:"attachmentID"`
	}{attachment.id})
	data, _ := marshalWire(response)
	if _, err := connection.Write(data); err != nil {
		return
	}
	_ = connection.SetDeadline(time.Time{})
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 4096), recordLimit)
	for scanner.Scan() {
		var frame hostFrame
		if strictDecode(scanner.Bytes(), &frame, recordLimit) != nil || frame.Schema != 1 || frame.Runtime != s.ref || frame.AttachmentID != attachment.id || frame.Event != "host_result" || !uuidPattern.MatchString(frame.ID) || frame.Call != nil || frame.Reply == nil || !validHostReply(*frame.Reply) {
			return
		}
		s.callbackMu.Lock()
		waiter := attachment.pending[frame.ID]
		if waiter != nil {
			delete(attachment.pending, frame.ID)
		}
		s.callbackMu.Unlock()
		if waiter != nil {
			waiter <- *frame.Reply
		}
	}
}

func (s *supervisor) availableHostTools(subject string, policy *ToolPolicyTarget) []string {
	s.callbackMu.Lock()
	defer s.callbackMu.Unlock()
	if s.attachment == nil {
		return nil
	}
	var tools []string
	for _, name := range s.attachment.tools {
		if subject == "utility" {
			if name == "monitor_operation" {
				tools = append(tools, name)
			}
		} else if name != "monitor_operation" && (name != "publish_plan" || policy != nil) && (policy == nil || slices.Contains(policy.RegisteredCeiling, name)) {
			tools = append(tools, name)
		}
	}
	return tools
}
func (s *supervisor) forwardHost(ctx context.Context, call HostCall, captured []string) (HostReply, bool) {
	s.callbackMu.Lock()
	attachment := s.attachment
	if attachment == nil || !slices.Contains(captured, call.Tool) || !slices.Contains(attachment.tools, call.Tool) {
		s.callbackMu.Unlock()
		return HostReply{Outcome: "refused", Code: "unavailable"}, false
	}
	if len(attachment.pending) >= 128 {
		s.callbackMu.Unlock()
		return HostReply{Outcome: "refused", Code: "unavailable"}, false
	}
	id := newUUID()
	waiter := make(chan HostReply, 1)
	attachment.pending[id] = waiter
	s.callbackMu.Unlock()
	defer func() { s.callbackMu.Lock(); delete(attachment.pending, id); s.callbackMu.Unlock() }()
	frame := hostFrame{Schema: 1, AttachmentID: attachment.id, Runtime: s.ref, Event: "host_call", ID: id, Call: &call}
	data, err := marshalWire(frame)
	if err != nil {
		return HostReply{Outcome: "refused", Code: "invalid"}, false
	}
	attachment.writeMu.Lock()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	_ = attachment.connection.SetWriteDeadline(deadline)
	_, err = attachment.connection.Write(data)
	_ = attachment.connection.SetWriteDeadline(time.Time{})
	attachment.writeMu.Unlock()
	if err != nil {
		return HostReply{Outcome: "unknown", Code: "unknown"}, true
	}
	select {
	case reply := <-waiter:
		return reply, true
	case <-ctx.Done():
		cancel := hostFrame{Schema: 1, AttachmentID: attachment.id, Runtime: s.ref, Event: "host_cancel", ID: id}
		data, _ := marshalWire(cancel)
		attachment.writeMu.Lock()
		_ = attachment.connection.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = attachment.connection.Write(data)
		_ = attachment.connection.SetWriteDeadline(time.Time{})
		attachment.writeMu.Unlock()
		return HostReply{Outcome: "unknown", Code: "canceled"}, true
	case <-attachment.closed:
		return HostReply{Outcome: "unknown", Code: "unknown"}, true
	}
}

func (s *supervisor) ownedHostCall(ctx context.Context, c *conversation, identity workerIdentity, captured []string, native nativeHostCall) (HostReply, error) {
	c.activeMu.Lock()
	ref := c.activeRef
	unknown := c.callbackUnknown
	w := c.worker
	c.activeMu.Unlock()
	if unknown || ref == nil || ref.id() != native.InputOperationID || !c.matchesReceipt(*ref) || identity.Generation != s.ref.Generation || w == nil {
		return HostReply{Outcome: "refused", Code: "stale"}, nil
	}
	sum := sha256.Sum256(native.Arguments)
	if len(native.Arguments) > callbackLimit || !json.Valid(native.Arguments) || hex.EncodeToString(sum[:]) != native.ArgumentsSHA256 || native.NativeToolCallID == "" || len(native.NativeToolCallID) > 4096 {
		return HostReply{Outcome: "refused", Code: "invalid"}, nil
	}
	for {
		status, err := c.receipts.loadOwned(ref.id())
		if err != nil || !status.ref.equal(*ref) {
			return HostReply{Outcome: "unknown", Code: "unknown"}, ErrHeld
		}
		if status.Phase == "Accepted" && status.Witness != nil {
			break
		}
		if status.Phase != "prepared" && status.Phase != "dispatching" {
			return HostReply{Outcome: "refused", Code: "unknown"}, ErrHeld
		}
		select {
		case <-ctx.Done():
			return HostReply{Outcome: "unknown", Code: "canceled"}, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	var origin struct {
		Verified                                   bool
		RootEntryID, AnchorEntryID, SelectionToken string
	}
	if err := w.Call(ctx, "verify_origin", native, &origin); err != nil || !origin.Verified {
		return HostReply{Outcome: "refused", Code: "stale"}, ErrScope
	}
	subject := identity.subject()
	token, err := LinearSelectionToken(subject, origin.RootEntryID)
	if err != nil || token != native.SelectionToken || origin.SelectionToken != token || origin.AnchorEntryID != native.AnchorEntryID {
		return HostReply{Outcome: "refused", Code: "stale"}, ErrScope
	}
	call := HostCall{Runtime: s.ref, Subject: subject, InputOperationID: native.InputOperationID, AnchorEntryID: native.AnchorEntryID, NativeToolCallID: native.NativeToolCallID, SelectionToken: token, Tool: native.Tool, ArgumentsSHA256: native.ArgumentsSHA256, Arguments: native.Arguments}
	reply, forwarded := s.forwardHost(ctx, call, captured)
	if forwarded && reply.Outcome == "unknown" {
		c.activeMu.Lock()
		c.callbackUnknown = true
		c.activeMu.Unlock()
		go func() {
			holdConversation(c)
			c.mu.Lock()
			c.record.HeldReason = "forwarded callback outcome unknown"
			_ = c.save()
			c.mu.Unlock()
		}()
	}
	return reply, nil
}

// AttachHost owns only the private duplex attachment. Domain handlers own all
// durable idempotency and may finish after cancellation; no callback ledger.
func (m *Manager) AttachHost(ctx context.Context, ref RuntimeRef, handler HostCallbackHandler) error {
	if handler == nil {
		return ErrUnavailable
	}
	status, err := m.Inspect(ctx, ref)
	if err != nil || status.Phase != "ready" {
		return ErrHeld
	}
	var tools []string
	if provider, ok := handler.(HostToolProvider); ok {
		tools = append([]string{}, provider.HostTools()...)
	}
	if !hostToolsValid(tools) {
		return ErrScope
	}
	directory, err := os.Open(m.instance(ref))
	if err != nil {
		return err
	}
	defer directory.Close()
	connection, err := m.config.Dial(ctx, filepath.Join("/proc/self/fd", strconv.Itoa(int(directory.Fd())), "control.sock"))
	if err != nil {
		return err
	}
	defer connection.Close()
	// Setup must obey the same cancellation bound as the attached connection.
	// The peer may accept the socket without ever acknowledging host_attach.
	stopClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopClose()
	deadline := time.Now().Add(30 * time.Second)
	if requested, ok := ctx.Deadline(); ok && requested.Before(deadline) {
		deadline = requested
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return err
	}
	uid, err := peerUID(connection)
	if err != nil || uid != uint32(os.Geteuid()) {
		return ErrScope
	}
	body, _ := json.Marshal(struct {
		Tools []string `json:"tools"`
	}{tools})
	request := controlRequest{Schema: 1, ID: newUUID(), Runtime: ref, Operation: "host_attach", Body: body}
	data, _ := marshalWire(request)
	if _, err := connection.Write(data); err != nil {
		return err
	}
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 4096), recordLimit)
	if !scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrUnavailable
	}
	var response controlResponse
	if strictDecode(scanner.Bytes(), &response, recordLimit) != nil || response.Runtime != ref || response.ID != request.ID || response.Schema != 1 || response.Error != "" {
		return ErrScope
	}
	var attached struct {
		ID string `json:"attachmentID"`
	}
	if strictDecode(response.Body, &attached, 4096) != nil || !uuidPattern.MatchString(attached.ID) {
		return ErrScope
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return err
	}
	attachmentCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writeMu sync.Mutex
	var mu sync.Mutex
	pending := map[string]context.CancelFunc{}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, cancel := range pending {
			cancel()
		}
	}()
	for scanner.Scan() {
		var frame hostFrame
		if strictDecode(scanner.Bytes(), &frame, recordLimit) != nil || frame.Schema != 1 || frame.Runtime != ref || frame.AttachmentID != attached.ID || !uuidPattern.MatchString(frame.ID) {
			return ErrScope
		}
		if frame.Event == "host_cancel" {
			if frame.Call != nil || frame.Reply != nil {
				return ErrScope
			}
			mu.Lock()
			cancel := pending[frame.ID]
			mu.Unlock()
			if cancel != nil {
				cancel()
			}
			continue
		}
		if frame.Event != "host_call" || frame.Call == nil || frame.Reply != nil || validateHostCall(ref, *frame.Call) != nil || !slices.Contains(tools, frame.Call.Tool) {
			return ErrScope
		}
		mu.Lock()
		if _, exists := pending[frame.ID]; exists || len(pending) >= 128 {
			mu.Unlock()
			return ErrScope
		}
		callCtx, callCancel := context.WithTimeout(attachmentCtx, 30*time.Second)
		pending[frame.ID] = callCancel
		mu.Unlock()
		go func(frame hostFrame) {
			defer callCancel()
			defer func() { mu.Lock(); delete(pending, frame.ID); mu.Unlock() }()
			reply, err := handler.HandleHostCall(callCtx, *frame.Call)
			if err != nil || !validHostReply(reply) {
				reply = HostReply{Outcome: "unknown", Code: "unknown"}
			}
			out := hostFrame{Schema: 1, AttachmentID: attached.ID, Runtime: ref, Event: "host_result", ID: frame.ID, Reply: &reply}
			data, err := marshalWire(out)
			if err != nil {
				return
			}
			writeMu.Lock()
			deadline, ok := callCtx.Deadline()
			if !ok {
				deadline = time.Now().Add(time.Second)
			}
			_ = connection.SetWriteDeadline(deadline)
			_, _ = connection.Write(data)
			_ = connection.SetWriteDeadline(time.Time{})
			writeMu.Unlock()
		}(frame)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("Pi host attachment closed")
}

var _ HostCallbackConnector = (*Manager)(nil)

func validateHostCall(ref RuntimeRef, call HostCall) error {
	if call.Runtime != ref || !operationPattern.MatchString(call.InputOperationID) || call.AnchorEntryID == "" || len(call.AnchorEntryID) > 4096 || !validString(call.AnchorEntryID) || call.NativeToolCallID == "" || len(call.NativeToolCallID) > 4096 || !validString(call.NativeToolCallID) || len(call.Arguments) > callbackLimit || !json.Valid(call.Arguments) {
		return ErrScope
	}
	sum := sha256.Sum256(call.Arguments)
	if hex.EncodeToString(sum[:]) != call.ArgumentsSHA256 {
		return ErrConflict
	}
	const prefix = "linear-v1:"
	if len(call.SelectionToken) != len(prefix)+64 || call.SelectionToken[:len(prefix)] != prefix || !hexPattern.MatchString(call.SelectionToken[len(prefix):]) {
		return ErrScope
	}
	switch call.Subject.Kind {
	case "conversation":
		if call.Subject.Conversation == nil || call.Subject.Utility != nil {
			return ErrScope
		}
		h := *call.Subject.Conversation
		if validateHandle(h) != nil || h.Conversation.Session != ref.Session || h.Generation != ref.Generation {
			return ErrScope
		}
	case "utility":
		if call.Subject.Utility == nil || call.Subject.Conversation != nil || call.Tool != "monitor_operation" {
			return ErrScope
		}
		u := call.Subject.Utility
		if u.Session != ref.Session || u.Generation != ref.Generation || !operationPattern.MatchString(u.MonitorOperationID) || !hexPattern.MatchString(u.Incarnation) || !uuidPattern.MatchString(u.NativeID) {
			return ErrScope
		}
	default:
		return ErrScope
	}
	return nil
}
