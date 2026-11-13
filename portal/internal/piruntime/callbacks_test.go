package piruntime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type callbackFixtureHandler struct {
	tools []string
	mu    sync.Mutex
	calls []HostCall
	check func(context.Context, HostCall) error
}

func (h *callbackFixtureHandler) HostTools() []string { return h.tools }
func (h *callbackFixtureHandler) HandleHostCall(ctx context.Context, call HostCall) (HostReply, error) {
	if h.check != nil {
		if err := h.check(ctx, call); err != nil {
			return HostReply{}, err
		}
	}
	h.mu.Lock()
	h.calls = append(h.calls, call)
	h.mu.Unlock()
	var args struct {
		ID string `json:"message_id"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	body, _ := json.Marshal(map[string]any{"disposition": "attempted", "outcome": "accepted", "native": map[string]any{"Outcome": "accepted", "Receipt": map[string]string{"TurnID": "owned-domain-entry", "ClientUserMessageID": args.ID}}})
	return HostReply{Outcome: "result", Body: body}, nil
}
func attachFixture(t *testing.T, m *Manager, s *supervisor, ctx context.Context, ref RuntimeRef, handler HostCallbackHandler) context.CancelFunc {
	t.Helper()
	attachCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- m.AttachHost(attachCtx, ref, handler) }()
	for {
		s.callbackMu.Lock()
		ready := s.attachment != nil
		s.callbackMu.Unlock()
		if ready {
			break
		}
		select {
		case err := <-done:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("owned attachment did not close")
		}
	})
	return cancel
}

func TestActualNativeHostCallbacksGateOnOriginalReceiptWithoutSendDeadlock(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	handler := &callbackFixtureHandler{tools: []string{"report_to_lead"}}
	message := "single original native input"
	handler.check = func(ctx context.Context, call HostCall) error {
		if call.Subject.Kind != "conversation" || call.Subject.Conversation == nil || call.Subject.Utility != nil || call.Subject.Conversation.Generation != runtime.Runtime.Generation || call.InputOperationID != "44444444-4444-4444-8444-444444444444" || call.AnchorEntryID == "" || call.NativeToolCallID == "" {
			return ErrScope
		}
		sum := sha256.Sum256(call.Arguments)
		if hex.EncodeToString(sum[:]) != call.ArgumentsSHA256 {
			return ErrConflict
		}
		ref := SubmissionRef{Conversation: call.Subject.Conversation.Conversation, OperationID: call.InputOperationID, Action: "send"}
		ref.Digest = SubmissionDigest(ref, SendInput{Message: message}, settingsFixture())
		status, err := m.Lookup(ctx, ref)
		if err != nil || status.Phase != "Accepted" || status.Witness == nil {
			return ErrHeld
		}
		return nil
	}
	attachFixture(t, m, h.s, ctx, runtime.Runtime, handler)
	state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: "22222222-2222-4222-8222-222222222222", Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	result, err := m.Deliver(ctx, handle, backend.SendRequest{Message: message, ClientUserMessageID: "44444444-4444-4444-8444-444444444444"})
	if err != nil || result.Outcome != backend.Accepted {
		c, _ := h.s.member(handle.Conversation)
		entries, _ := os.ReadDir(c.receipts.root)
		t.Log("receipt files", entries)
		data, _ := os.ReadFile(filepath.Join(c.root, "agent/fixture-error.log"))
		t.Log("fixture-only error", string(data))
		t.Fatal(result, err)
	}
	waitIdle(t, m, ctx, handle)
	handler.mu.Lock()
	calls := append([]HostCall{}, handler.calls...)
	handler.mu.Unlock()
	if len(calls) != 2 || calls[0].AnchorEntryID != calls[1].AnchorEntryID || calls[0].NativeToolCallID == calls[1].NativeToolCallID || calls[0].SelectionToken != calls[1].SelectionToken {
		t.Fatal("native tool invocation provenance", calls)
	}
	c, _ := h.s.member(handle.Conversation)
	before, err := os.ReadFile(c.record.HistoryFile)
	if err != nil {
		t.Fatal(err)
	}
	origin := NativeOriginRef{AnchorEntryID: calls[0].AnchorEntryID, SelectionToken: calls[0].SelectionToken, NativeToolCallID: calls[0].NativeToolCallID, Tool: calls[0].Tool}
	observed, err := m.InspectNativeOrigin(ctx, handle, origin)
	var expectedArgs, observedArgs any
	expectedErr := json.Unmarshal(calls[0].Arguments, &expectedArgs)
	observedErr := json.Unmarshal(observed.Arguments, &observedArgs)
	if err != nil || expectedErr != nil || observedErr != nil || observed.Handle != handle || observed.Origin != origin || !reflect.DeepEqual(observedArgs, expectedArgs) {
		diagnostic, _ := os.ReadFile(filepath.Join(c.root, "agent/fixture-error.log"))
		t.Log("owned native origin diagnostic", string(diagnostic))
		t.Fatal("historical inspection after active input settled", observed, err)
	}
	after, err := os.ReadFile(c.record.HistoryFile)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("inspection changed history", err)
	}
	badOrigin := origin
	badOrigin.AnchorEntryID = "missing-anchor"
	if _, err := m.InspectNativeOrigin(ctx, handle, badOrigin); err == nil {
		t.Fatal("wrong historical anchor accepted")
	}
	stale := handle
	stale.Generation = strings.Repeat("d", 64)
	if _, err := m.InspectNativeOrigin(ctx, stale, origin); !errors.Is(err, ErrScope) {
		t.Fatal("stale historical generation accepted", err)
	}
	for _, mutate := range []func(*HostCall){
		func(call *HostCall) { call.InputOperationID = "../bad" },
		func(call *HostCall) { call.Arguments = json.RawMessage(`{"changed":true}`) },
		func(call *HostCall) { call.Subject.Kind = "utility" },
		func(call *HostCall) { call.Subject.Conversation = nil },
		func(call *HostCall) { call.SelectionToken = "linear-v1:invalid" },
		func(call *HostCall) { call.Runtime.Generation = strings.Repeat("d", 64) },
	} {
		bad := calls[0]
		mutate(&bad)
		if validateHostCall(runtime.Runtime, bad) == nil {
			t.Fatal("invalid callback accepted", bad)
		}
	}
	if err := m.AttachHost(ctx, runtime.Runtime, handler); err == nil {
		t.Fatal("second attachment admitted")
	}
	if stop, err := m.Unload(ctx, handle); err != nil || !stop.VerifiedExit {
		t.Fatal(stop, err)
	}
	if _, err := m.InspectNativeOrigin(ctx, handle, origin); !errors.Is(err, ErrUnavailable) || c.worker != nil {
		t.Fatal("cold inspection loaded a worker", err)
	}
}

func TestAttachHostSetupCancellationBeforeAcknowledgementClosesConnection(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			m, _, start, ctx := supervisorFixture(t)
			runtime, err := m.Ensure(ctx, start)
			if err != nil {
				t.Fatal(err)
			}
			directory, err := os.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer directory.Close()
			path := filepath.Join("/proc/self/fd", fmtInt(int(directory.Fd())), "withheld.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			entered, peerDone := make(chan struct{}), make(chan error, 1)
			go func() {
				peer, err := listener.AcceptUnix()
				if err != nil {
					peerDone <- err
					return
				}
				defer peer.Close()
				_ = peer.SetDeadline(time.Now().Add(2 * time.Second))
				scanner := bufio.NewScanner(peer)
				if !scanner.Scan() {
					peerDone <- ErrUnavailable
					return
				}
				close(entered)
				_, err = io.Copy(io.Discard, peer)
				peerDone <- err
			}()
			originalDial := m.config.Dial
			dials := 0
			m.config.Dial = func(ctx context.Context, original string) (net.Conn, error) {
				dials++
				if dials == 2 {
					return originalDial(ctx, path)
				}
				return originalDial(ctx, original)
			}
			attachCtx, cancel := context.WithCancel(ctx)
			if deadline {
				cancel()
				attachCtx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
			}
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- m.AttachHost(attachCtx, runtime.Runtime, &callbackFixtureHandler{}) }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatal("attach did not reach withheld ACK", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if !deadline {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil || (deadline && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrUnavailable)) || (!deadline && !errors.Is(err, context.Canceled)) {
					t.Fatal("setup cancellation lost", err)
				}
			case <-time.After(time.Second):
				t.Fatal("setup did not terminate")
			}
			select {
			case err := <-peerDone:
				if err != nil {
					t.Fatal("connection did not drain after setup cancellation", err)
				}
			case <-time.After(time.Second):
				t.Fatal("setup connection remained open")
			}
		})
	}
}

func TestCallbackProjectionAndRawArgumentsAreBounded(t *testing.T) {
	if hostToolsValid([]string{"unshipped"}) || hostToolsValid([]string{"report_to_lead", "report_to_lead"}) || hostToolsValid([]string{"team_message", "report_to_lead"}) {
		t.Fatal("invalid advertised set")
	}
	argument := json.RawMessage(`{"text":"<é>"}`)
	data, err := marshalWire(HostCall{Arguments: argument})
	if err != nil {
		t.Fatal(err)
	}
	var call HostCall
	if err := strictDecode(data, &call, recordLimit); err != nil {
		t.Fatal(err)
	}
	if string(call.Arguments) != string(argument) {
		t.Fatal("argument bytes changed", string(call.Arguments))
	}
	if validHostReply(HostReply{Outcome: "result", Body: json.RawMessage(strings.Repeat("x", callbackLimit+1))}) {
		t.Fatal("oversized host result")
	}
	s := &supervisor{attachment: &hostAttachment{tools: []string{"report_to_lead"}, pending: map[string]chan HostReply{}}}
	for i := 0; i < 128; i++ {
		s.attachment.pending[fmtInt(i)] = make(chan HostReply, 1)
	}
	if reply, forwarded := s.forwardHost(context.Background(), HostCall{Tool: "report_to_lead"}, []string{"report_to_lead"}); forwarded || reply.Outcome != "refused" {
		t.Fatal("overflow forwarded", reply)
	}
}

func TestForwardedCallbackCancellationAndDisconnectRetainUnknown(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "abort", true: "disconnect"}[disconnect], func(t *testing.T) {
			m, h, start, ctx := supervisorFixture(t)
			runtime, err := m.Ensure(ctx, start)
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			finished := make(chan struct{})
			var enterOnce, finishOnce sync.Once
			handler := &callbackFixtureHandler{tools: []string{"report_to_lead"}, check: func(ctx context.Context, _ HostCall) error {
				enterOnce.Do(func() { close(entered) })
				<-ctx.Done()
				finishOnce.Do(func() { close(finished) })
				return ctx.Err()
			}}
			detach := attachFixture(t, m, h.s, ctx, runtime.Runtime, handler)
			state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: "22222222-2222-4222-8222-222222222222", Settings: settingsFixture()})
			if err != nil {
				t.Fatal(err)
			}
			handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
			result, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "one original invocation", ClientUserMessageID: "44444444-4444-4444-8444-444444444444"})
			if err != nil || result.Outcome != backend.Accepted {
				t.Fatal(result, err)
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if disconnect {
				detach()
			} else {
				operations, err := m.Bind(ctx, handle)
				if err != nil {
					t.Fatal(err)
				}
				if err := operations.Interrupt.Interrupt(ctx, BackendIdentity(handle)); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("handler not canceled", ctx.Err())
			}
			c, _ := h.s.member(handle.Conversation)
			for {
				c.mu.Lock()
				reason := c.record.HeldReason
				c.mu.Unlock()
				if reason != "" {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("callback uncertainty not persisted")
				case <-time.After(time.Millisecond):
				}
			}
			if _, err := m.Unload(ctx, handle); err == nil {
				t.Fatal("unknown callback silently drained")
			}
			if next, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "must not dispatch", ClientUserMessageID: "55555555-5555-4555-8555-555555555555"}); err == nil || next.Outcome != backend.Unknown {
				t.Fatal(next, err)
			}
			c.activeMu.Lock()
			unknown := c.callbackUnknown
			c.activeMu.Unlock()
			if !unknown {
				t.Fatal("forwarded outcome lost")
			}
		})
	}
}
