package web

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func conflictDomainFixture(t *testing.T) (*piTeamConflictDomain, *piAddedTeamFixture, []piruntime.LifecycleMember, piruntime.TeamConflictCall) {
	t.Helper()
	ctx := context.Background()
	s, receipt, root := newPiCreationFixtureWithPreset(t, "2026-11-25-team-conflict", historicalPiTeamPreset())
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal("domain fixture stage", err)
	}
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		t.Fatal("domain fixture stage", err)
	}
	roster, err := store.LoadPi("2026-11-25-team-conflict")
	if err != nil {
		t.Fatal("domain fixture stage", err)
	}
	member := roster.Members[0]
	caller := teamruntime.PiCaller{Runtime: root.runtime, Handle: agentbackend.ExecutionHandle{Conversation: root.state.Conversation, Generation: root.state.Generation}}
	message := teamruntime.PiMessage{ID: "44444444-4444-4444-8444-444444444444", To: member.Address, Text: "reserved original"}
	if _, err := store.ReservePiInitialAssignment(ctx, s, caller, roster.Revision, message, receipt.Pi.Selection.Members[1].Settings.Effective()); err != nil {
		t.Fatal("reserve", err)
	}
	roster, err = store.LoadPi("2026-11-25-team-conflict")
	if err != nil {
		t.Fatal("domain fixture stage", err)
	}
	current, err := s.piReadySessionIdentityLocked(root.runtime.Session)
	if err != nil {
		t.Fatal("ready identity", err)
	}
	d := &piTeamConflictDomain{server: s, current: current, roster: roster}
	ports := &piAddedTeamFixture{piSettingsFixture: &piSettingsFixture{piObservedFixture: &piObservedFixture{piCreationRuntimeFixture: root}}, slots: map[string]*piSettingsFixture{}, creation: map[string]string{}, receipts: map[string]piruntime.SubmissionStatus{}}
	s.config.PiCreation.Runtime = ports
	target, err := ports.Create(ctx, root.runtime, piruntime.CreateRequest{Session: root.runtime.Session, Member: member.Address, Incarnation: member.Incarnation, CreationID: member.CreationID, ToolPolicy: root.initialPolicy, Settings: receipt.Pi.Selection.Members[1].Settings.Effective()})
	if err != nil {
		t.Fatal("target Create", err)
	}
	slots, err := d.Slots(ctx, root.runtime)
	if err != nil {
		t.Fatal("domain Slots", err)
	}
	members := []piruntime.LifecycleMember{{Slot: slots[0], Conversation: &root.state.Conversation}, {Slot: slots[1], Conversation: &target.Conversation}}
	args := json.RawMessage(`{"message_id":"55555555-5555-4555-8555-555555555555","to":"implementer0","message":"one immutable message"}`)
	call := piruntime.TeamConflictCall{LifecycleCallback: piruntime.LifecycleCallback{Conversation: root.state.Conversation, Input: root.submission.Ref, Origin: piruntime.NativeOriginRef{AnchorEntryID: "anchor", SelectionToken: "selection", NativeToolCallID: "call", Tool: "team_message"}, ArgumentsSHA256: conflictDigestRaw(args), MessageID: "55555555-5555-4555-8555-555555555555", To: member.Address}, Arguments: args}
	return d, ports, members, call
}
func TestPiTeamConflictDomainUsesOriginalSettlementOnly(t *testing.T) {
	d, ports, members, call := conflictDomainFixture(t)
	ctx := context.Background()
	source := ports.runtime
	creates, sends := ports.creates, ports.sends
	before := privateWebInventory(t, d.server.config.UserStateRoot)
	proof, err := d.Settle(ctx, source, call, members)
	if err != nil || proof.TeamConflictEvidence().Kind != "initial_conflict" {
		t.Fatal(proof, err)
	}
	if !reflect.DeepEqual(before, privateWebInventory(t, d.server.config.UserStateRoot)) {
		t.Fatal("read proof mutated domain")
	}
	for _, phase := range []string{"Accepted", "NotSubmitted", "Unknown"} {
		evidence := proof.TeamConflictEvidence()
		ref := piruntime.SubmissionRef{Conversation: evidence.Target, OperationID: call.MessageID, Action: "send"}
		settings := ports.slots[ref.Conversation.Member].state.Settings
		ref.Digest = piruntime.SubmissionDigest(ref, evidence.Input, settings)
		status := piruntime.SubmissionStatus{Schema: 1, Ref: ref, Phase: phase, Generation: source.Generation, Settings: settings, Input: evidence.Input}
		ports.receipts[call.MessageID] = status
		got, err := d.Settle(ctx, source, call, members)
		if phase == "Unknown" {
			if err == nil {
				t.Fatal("unknown accepted")
			}
			continue
		}
		if err != nil || got.TeamConflictEvidence().Kind != map[string]string{"Accepted": "accepted", "NotSubmitted": "not_submitted"}[phase] {
			t.Fatal(got, err)
		}
		status.Input.Message += "changed"
		ports.receipts[call.MessageID] = status
		if _, err := d.Settle(ctx, source, call, members); !errors.Is(err, piruntime.ErrConflict) {
			t.Fatal("changed immutable receipt", err)
		}
	}
	delete(ports.receipts, call.MessageID)
	call.MessageID = members[1].Slot.InitialInputID
	call.Arguments = json.RawMessage(`{"message_id":"44444444-4444-4444-8444-444444444444","to":"implementer0","message":"original reserved body"}`)
	call.ArgumentsSHA256 = conflictDigestRaw(call.Arguments)
	proof, err = d.Settle(ctx, source, call, members)
	if err != nil || proof.TeamConflictEvidence().Kind != "created_empty" {
		t.Fatal(proof, err)
	}
	if ports.creates != creates || ports.sends != sends || ports.slots["implementer0"].sends != 0 {
		t.Fatal("domain proof sent native input")
	}
}
func TestPiTeamConflictHistoricalPublicationsDoNotRequireLatestOrAnswer(t *testing.T) {
	for _, tool := range []string{"request_user_input_async", "publish_plan"} {
		t.Run(tool, func(t *testing.T) {
			d, ports, members, call := conflictDomainFixture(t)
			ctx := context.Background()
			call.Origin.Tool = tool
			call.MessageID = ""
			call.To = ""
			caller := piquestions.Caller{Handle: agentbackend.ExecutionHandle{Conversation: call.Conversation, Generation: strings.Repeat("f", 64)}, Lineage: piquestions.LineageRef{AnchorEntryID: call.Origin.AnchorEntryID, SelectionToken: call.Origin.SelectionToken}, NativeToolCallID: call.Origin.NativeToolCallID}
			validate := func(context.Context, agentbackend.ConversationRef, *piquestions.Caller) error { return nil }
			var pending func() error
			if tool == "publish_plan" {
				store := piplan.FileStore{Root: d.server.config.UserStateRoot, Validate: validate, Policy: func(context.Context, agentbackend.ConversationRef, piplan.Caller) (piplan.Policy, error) {
					return piplan.Policy{OriginalMode: "plan", Revision: "proof", Idle: true, Eligible: true, Enforced: true}, nil
				}}
				_, err := store.PublishProposal(ctx, caller, "original plan", piplan.NativeOrigin{EntryID: "anchor", Complete: true})
				if err != nil {
					t.Fatal(err)
				}
				newer := caller
				newer.NativeToolCallID = "newer-call"
				newer.Lineage.AnchorEntryID = "newer-anchor"
				latest, err := store.PublishProposal(ctx, newer, "newer plan", piplan.NativeOrigin{EntryID: "newer-anchor", Complete: true})
				if err != nil {
					t.Fatal(err)
				}
				call.Arguments = json.RawMessage(`{"text":"original plan"}`)
				pending = func() error {
					_, err := store.Accept(ctx, call.Conversation, piplan.AcceptanceInput{AcceptanceID: "11111111-1111-4111-8111-111111111111", PlanID: latest.PlanID, Revision: latest.Revision, TextSHA256: latest.TextSHA256})
					return err
				}
			} else {
				store := piquestions.FileStore{Root: d.server.config.UserStateRoot, Validate: validate}
				original, err := store.Publish(ctx, caller, piquestions.PublishInput{Questions: []piquestions.Question{{Title: "Original unanswered question"}}})
				if err != nil {
					t.Fatal(err)
				}
				call.Arguments = json.RawMessage(`{"questions":[{"title":"Original unanswered question"}]}`)
				pending = func() error {
					_, err := store.AcceptResponse(ctx, call.Conversation, piquestions.ResponseInput{ResponseID: "11111111-1111-4111-8111-111111111111", QuestionID: original.ID, QuestionRevision: original.Revision, Answers: []piquestions.Answer{{QuestionID: "0", Text: "answer"}}})
					return err
				}
			}
			call.ArgumentsSHA256 = conflictDigestRaw(call.Arguments)
			proof, err := d.Settle(ctx, ports.runtime, call, members)
			if err != nil || proof == nil {
				t.Fatal(proof, err)
			}
			originalArguments := append(json.RawMessage(nil), call.Arguments...)
			call.Arguments = json.RawMessage(`{"text":"changed body"}`)
			call.ArgumentsSHA256 = conflictDigestRaw(call.Arguments)
			if _, err := d.Settle(ctx, ports.runtime, call, members); err == nil {
				t.Fatal("changed publication body accepted")
			}
			call.Arguments = originalArguments
			call.ArgumentsSHA256 = conflictDigestRaw(call.Arguments)
			if err := pending(); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Settle(ctx, ports.runtime, call, members); !errors.Is(err, piruntime.ErrHeld) {
				t.Fatal("pending separate delivery treated as settled publication", err)
			}
			call.Origin.NativeToolCallID = "missing-call"
			if _, err := d.Settle(ctx, ports.runtime, call, members); err == nil {
				t.Fatal("missing durable publication accepted")
			}
		})
	}
}

func privateWebInventory(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[path] = string(raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPiTeamConflictDomainAcceptsOnlyProjectedTerminalPolicyRefusal(t *testing.T) {
	for _, tool := range []string{"team_message", "report_to_lead"} {
		t.Run(tool, func(t *testing.T) {
			d, ports, members, call := conflictDomainFixture(t)
			ports.creation["lead"] = members[0].Slot.CreationID
			call.To = "lead"
			call.MessageID = "77777777-7777-4777-8777-777777777777"
			call.Arguments = json.RawMessage(`{"message_id":"77777777-7777-4777-8777-777777777777","to":"lead","message":"earlier synthetic callback"}`)
			call.ArgumentsSHA256 = conflictDigestRaw(call.Arguments)
			call.TerminalRefusal = &piruntime.TeamConflictTerminalRefusal{ResultEntryID: "result", NativeToolCallID: call.Origin.NativeToolCallID, Tool: "team_message", MessageSHA256: strings.Repeat("a", 64)}
			call.Origin.Tool = tool
			call.TerminalRefusal.Tool = tool
			if tool == "report_to_lead" {
				call.Arguments = json.RawMessage(`{"message_id":"77777777-7777-4777-8777-777777777777","message":"earlier synthetic callback"}`)
				call.ArgumentsSHA256 = conflictDigestRaw(call.Arguments)
			}
			before := privateWebInventory(t, d.server.config.UserStateRoot)
			proof, err := d.Settle(context.Background(), ports.runtime, call, members)
			if err != nil || proof.TeamConflictEvidence().Kind != "terminal_policy_refused" || !reflect.DeepEqual(proof.TeamConflictEvidence().TerminalRefusal, call.TerminalRefusal) {
				t.Fatal("terminal refusal not settled", proof, err)
			}
			if !reflect.DeepEqual(before, privateWebInventory(t, d.server.config.UserStateRoot)) {
				t.Fatal("proof mutated state")
			}
			for _, phase := range []string{"Accepted", "NotSubmitted", "Unknown"} {
				evidence := proof.TeamConflictEvidence()
				settings := ports.state.Settings
				ref := piruntime.SubmissionRef{Conversation: evidence.Target, OperationID: call.MessageID, Action: "send"}
				ref.Digest = piruntime.SubmissionDigest(ref, evidence.Input, settings)
				now := time.Now().UTC()
				status := piruntime.SubmissionStatus{Schema: 1, Ref: ref, Phase: phase, Generation: ports.runtime.Generation, Settings: settings, Input: evidence.Input, CreatedAt: now, UpdatedAt: now}
				if phase == "Accepted" {
					status.Witness = &piruntime.NativeWitness{SessionID: ref.Conversation.NativeID, EntryID: "accepted-original-user", EffectiveDigest: piruntime.MessageDigest(status.Input.Message)}
				}
				if phase == "NotSubmitted" {
					status.Refusal = "unavailable"
				}
				ports.receipts[call.MessageID] = status
				got, err := d.Settle(context.Background(), ports.runtime, call, members)
				if phase == "Unknown" {
					if !errors.Is(err, piruntime.ErrHeld) {
						t.Fatal("unknown receipt accepted", got, err)
					}
				} else if err != nil || got.TeamConflictEvidence().Kind != map[string]string{"Accepted": "accepted", "NotSubmitted": "not_submitted"}[phase] || got.TeamConflictEvidence().TerminalRefusal != nil {
					t.Fatal("matching original receipt lost to refused retry", phase, got, err)
				}
				status.Input.Message += "changed"
				ports.receipts[call.MessageID] = status
				if _, err := d.Settle(context.Background(), ports.runtime, call, members); !errors.Is(err, piruntime.ErrConflict) {
					t.Fatal("changed immutable receipt accepted", err)
				}
			}
			delete(ports.receipts, call.MessageID)
			if tool == "report_to_lead" {
				call.To = "implementer0"
				if _, err := d.Settle(context.Background(), ports.runtime, call, members); !errors.Is(err, piruntime.ErrScope) {
					t.Fatal("report target was caller-selected", err)
				}
				call.To = "lead"
			}
			call.TerminalRefusal = nil
			if _, err := d.Settle(context.Background(), ports.runtime, call, members); !errors.Is(err, piruntime.ErrHeld) {
				t.Fatal("receipt absence accepted", err)
			}
		})
	}
}
