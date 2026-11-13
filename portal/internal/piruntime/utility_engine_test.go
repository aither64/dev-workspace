package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
)

type utilityFixtureAuthority struct {
	mu      sync.Mutex
	grant   UtilityGrant
	deny    bool
	actions []string
	reads   int
}

func (a *utilityFixtureAuthority) WithUtility(_ context.Context, runtime RuntimeRef, ref UtilityRef, action string, run func(UtilityGrant) error) error {
	a.mu.Lock()
	grant, deny := a.grant, a.deny
	a.actions = append(a.actions, action)
	a.mu.Unlock()
	if deny {
		return ErrHeld
	}
	if runtime != grant.Runtime || ref != grant.Utility {
		return ErrScope
	}
	return run(grant)
}
func (a *utilityFixtureAuthority) WithUtilityRead(_ context.Context, runtime RuntimeRef, ref UtilitySubmissionRef, run func() error) error {
	a.mu.Lock()
	grant := a.grant
	a.reads++
	a.mu.Unlock()
	if runtime != grant.Runtime || ref != utilitySubmission(grant) {
		return ErrScope
	}
	return run()
}

type utilityFixtureHost struct {
	check func(context.Context, HostCall) error
}

func (h utilityFixtureHost) HostTools() []string {
	return []string{"monitor_operation", "report_to_lead"}
}
func (h utilityFixtureHost) HandleHostCall(ctx context.Context, call HostCall) (HostReply, error) {
	if err := h.check(ctx, call); err != nil {
		return HostReply{}, err
	}
	body, _ := json.Marshal("77777777-7777-4777-8777-777777777777")
	return HostReply{Outcome: "result", Body: body}, nil
}

func TestUtilityCallbackUsesExactOriginalAcceptedReceiptAndUnknownBlocksDrain(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "report", true: "lost-reply"}[disconnect], func(t *testing.T) {
			m, h, start, ctx := supervisorFixture(t)
			runtime, err := m.Ensure(ctx, start)
			if err != nil {
				t.Fatal(err)
			}
			u := UtilityRef{Session: start.Session, OperationID: strings.Repeat("d", 32), Incarnation: strings.Repeat("e", 64)}
			authority := &utilityFixtureAuthority{grant: UtilityGrant{Runtime: runtime.Runtime, Utility: u, CreationID: strings.Repeat("a", 32), LaunchOperationID: strings.Repeat("b", 32), Settings: settingsFixture(), Input: SendInput{Message: "real original monitor report brief", ActionContext: "monitor-launch:" + u.OperationID}}}
			m.config.UtilityAuthority, m.config.UtilityReadAuthority = authority, authority
			calls := make(chan HostCall, 1)
			finished := make(chan struct{})
			handler := utilityFixtureHost{check: func(handlerCtx context.Context, call HostCall) error {
				defer close(finished)
				if call.Runtime != runtime.Runtime || call.Subject.Kind != "utility" || call.Subject.Conversation != nil || call.Subject.Utility == nil || call.Subject.Utility.MonitorOperationID != u.OperationID || call.Subject.Utility.Incarnation != u.Incarnation || call.Subject.Utility.NativeID != authority.grant.Utility.NativeID || call.InputOperationID != authority.grant.LaunchOperationID || call.Tool != "monitor_operation" {
					return ErrScope
				}
				status, err := m.LookupUtility(handlerCtx, utilitySubmission(authority.grant))
				if err != nil || status.Phase != "Accepted" || status.Witness == nil {
					return ErrHeld
				}
				calls <- call
				if disconnect {
					<-handlerCtx.Done()
					return handlerCtx.Err()
				}
				return nil
			}}
			detach := attachFixture(t, m, h.s, ctx, runtime.Runtime, handler)
			state, err := m.CreateUtility(ctx, runtime.Runtime, UtilityCreateRequest{Utility: u, CreationID: authority.grant.CreationID, Settings: authority.grant.Settings})
			if err != nil {
				t.Fatal(state, err)
			}
			authority.grant.Utility = state.Utility
			handle := UtilityExecutionHandle{Utility: state.Utility, Generation: state.Generation}
			ops, err := m.BindUtility(ctx, handle)
			if err != nil {
				t.Fatal(err)
			}
			ref := utilitySubmission(authority.grant)
			result, err := ops.Send.Send(ctx, UtilityIdentity(handle), backend.SendRequest{Message: authority.grant.Input.Message, ClientUserMessageID: ref.OperationID, ActionContext: authority.grant.Input.ActionContext})
			if err != nil || result.Outcome != backend.Accepted {
				t.Fatal(result, err)
			}
			select {
			case <-calls:
			case <-ctx.Done():
				c, _ := h.s.utility(state.Utility)
				var snapshot workerSnapshot
				diagnosticCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = c.worker.Call(diagnosticCtx, "snapshot", struct{}{}, &snapshot)
				for _, raw := range snapshot.Entries {
					var entry nativeEntry
					if json.Unmarshal(raw, &entry) == nil && entry.Message.Role == "toolResult" {
						t.Log("owned native tool result", messageText(entry.Message.Content))
					}
				}
				diagnostic, _ := os.ReadFile(filepath.Join(c.root, "agent/fixture-error.log"))
				t.Log("owned worker diagnostic", string(diagnostic))
				t.Fatal("utility callback did not reach host")
			}
			c, _ := h.s.utility(state.Utility)
			if len(c.hostTools) != 1 || c.hostTools[0] != "monitor_operation" {
				t.Fatal("utility gained retained factory", c.hostTools)
			}
			if disconnect {
				detach()
				select {
				case <-finished:
				case <-ctx.Done():
					t.Fatal("handler did not cancel")
				}
				for {
					c.mu.Lock()
					held := c.record.HeldReason != ""
					c.mu.Unlock()
					if held {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("unknown not retained")
					case <-time.After(time.Millisecond):
					}
				}
				if _, err := m.UnloadUtility(ctx, handle); !errors.Is(err, ErrHeld) {
					t.Fatal("lost report reply drained", err)
				}
			} else {
				for {
					observed, err := m.LoadUtility(ctx, handle)
					if err != nil {
						t.Fatal(err)
					}
					if observed.State == "idle" {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-time.After(time.Millisecond):
					}
				}
				if stop, err := m.UnloadUtility(ctx, handle); err != nil || !stop.VerifiedExit {
					t.Fatal(stop, err)
				}
			}
			if status, err := m.LookupUtility(ctx, ref); err != nil || status.Phase != "Accepted" {
				t.Fatal("report outcome changed original input", status, err)
			}
		})
	}
}

func TestUtilityRealManagerSharedWorkerRequiresOriginalGrantAndReadAuthority(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	u := UtilityRef{Session: start.Session, OperationID: strings.Repeat("d", 32), Incarnation: strings.Repeat("e", 64)}
	authority := &utilityFixtureAuthority{grant: UtilityGrant{Runtime: runtime.Runtime, Utility: u, CreationID: strings.Repeat("a", 32), LaunchOperationID: strings.Repeat("b", 32), Settings: settingsFixture(), Input: SendInput{Message: "the one frozen monitor brief", ActionContext: "monitor-launch:" + u.OperationID}}}
	request := UtilityCreateRequest{Utility: u, CreationID: authority.grant.CreationID, Settings: authority.grant.Settings}
	if _, err := m.CreateUtility(ctx, runtime.Runtime, request); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing effect authority", err)
	}
	m.config.UtilityAuthority, m.config.UtilityReadAuthority = authority, authority
	state, err := m.CreateUtility(ctx, runtime.Runtime, request)
	if err != nil || state.State != "initialized" || state.Persisted || state.Utility.NativeID == "" || state.Generation != runtime.Runtime.Generation {
		t.Fatal(state, err)
	}
	authority.grant.Utility = state.Utility
	handle := UtilityExecutionHandle{Utility: state.Utility, Generation: state.Generation}
	identity := UtilityIdentity(handle)
	ops, err := m.BindUtility(ctx, handle)
	if err != nil || ops.Verify == nil || ops.Read == nil || ops.Send == nil || ops.Interrupt != nil {
		t.Fatal(ops, err)
	}
	if err := ops.Verify.Verify(ctx, identity); err != nil {
		t.Fatal(err)
	}
	ref := utilitySubmission(authority.grant)
	c, err := h.s.utility(state.Utility)
	if err != nil {
		t.Fatal(err)
	}
	if c.record.Conversation.NativeID != "" || c.record.OwnerKind != "utility" || !strings.Contains(c.root, "/utilities/"+u.OperationID+"/") {
		t.Fatal("retained masquerade", c.record)
	}
	if _, err := os.Stat(c.record.HistoryFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("initialization materialized history", err)
	}
	input := backend.SendRequest{Message: authority.grant.Input.Message, ClientUserMessageID: ref.OperationID, ActionContext: authority.grant.Input.ActionContext}
	for _, wrong := range []backend.SendRequest{
		{Message: "changed", ClientUserMessageID: input.ClientUserMessageID, ActionContext: input.ActionContext},
		{Message: input.Message, ClientUserMessageID: strings.Repeat("c", 32), ActionContext: input.ActionContext},
		{Message: input.Message, ClientUserMessageID: input.ClientUserMessageID, ActionContext: "initial"},
		{Message: input.Message, ClientUserMessageID: input.ClientUserMessageID, ActionContext: input.ActionContext, Retry: true},
	} {
		if _, err := ops.Send.Send(ctx, identity, wrong); !errors.Is(err, ErrConflict) {
			t.Fatal("changed original admitted", err)
		}
	}
	authority.deny = true
	if _, err := ops.Send.Send(ctx, identity, input); !errors.Is(err, ErrHeld) {
		t.Fatal("Bind became permanent permission", err)
	}
	if _, err := m.LookupUtility(ctx, ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused Send created receipt", err)
	}
	authority.deny = false
	result, err := ops.Send.Send(ctx, identity, input)
	if err != nil || result.Outcome != backend.Accepted || result.Receipt.ClientUserMessageID != input.ClientUserMessageID {
		t.Fatal(result, err)
	}
	status, err := m.LookupUtility(ctx, ref)
	if err != nil || status.Schema != 2 || status.OwnerKind != "utility" || status.Phase != "Accepted" || status.Ref != ref || status.Input != authority.grant.Input {
		t.Fatal(status, err)
	}
	if _, err := (&receiptEngine{root: c.receipts.root}).lookup(SubmissionRef{OperationID: ref.OperationID}); !errors.Is(err, ErrConflict) {
		t.Fatal("utility receipt read as retained", err)
	}
	for {
		state, err = m.LoadUtility(ctx, handle)
		if err != nil {
			t.Fatal(err)
		}
		if state.State == "idle" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if result, err := ops.Send.Send(ctx, identity, input); err != nil || result.Outcome != backend.Accepted {
		t.Fatal("same input receipt replay", result, err)
	}
	transcript, err := ops.Read.Read(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, entry := range transcript.Entries {
		if entry.Kind == "user" {
			users++
		}
	}
	if users != 1 {
		t.Fatal("original input redispatched", users)
	}
	authority.deny = true
	if _, err := m.LookupUtility(ctx, ref); err != nil {
		t.Fatal("read required effect permission", err)
	}
	if _, err := m.UnloadUtility(ctx, handle); !errors.Is(err, ErrHeld) {
		t.Fatal("unload bypassed grant", err)
	}
	authority.deny = false
	if stop, err := m.UnloadUtility(ctx, handle); err != nil || !stop.VerifiedExit {
		t.Fatal(stop, err)
	}
	if _, err := m.LookupUtility(ctx, ref); err != nil {
		t.Fatal("receipt lookup loaded worker", err)
	}
	if c.worker != nil {
		t.Fatal("lookup implicitly loaded worker")
	}
	if _, err := ops.Read.Read(ctx, identity); !errors.Is(err, ErrUnavailable) {
		t.Fatal("cold native read loaded worker", err)
	}
	stale := handle
	stale.Generation = strings.Repeat("f", 64)
	if _, err := m.LoadUtility(ctx, stale); !errors.Is(err, ErrScope) {
		t.Fatal("stale utility generation", err)
	}
	data, err := os.ReadFile(filepath.Join(c.root, "conversation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields["Conversation"] != nil || fields["HistoryRef"] != nil || fields["OwnerKind"] == nil {
		t.Fatal("utility mapping owner arm", string(data))
	}
}
