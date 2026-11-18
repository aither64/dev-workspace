package piruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestForkCodecRetainsLegacyAndRejectsBootstrapAlias(t *testing.T) {
	_, c, _, h := policySupervisorFixture(t)
	legacy, _ := json.Marshal(c.record)
	var decoded conversationRecord
	if strictDecode(legacy, &decoded, recordLimit) != nil {
		t.Fatal("legacy mapping rejected")
	}
	origin := ForkOrigin{Source: h, Snapshot: ForkSnapshot{Native: NativeForkSnapshot{SourceFile: filepath.Join(c.root, "history", "source.jsonl"), SourceNativeID: h.Conversation.NativeID, SourceCwd: h.Conversation.Session.Workspace, EntriesDigest: strings.Repeat("d", 64), EntryCount: 2, LeafID: "leaf"}, HistoryRef: c.record.HistoryRef, Settings: c.record.Settings, Instructions: c.record.Instructions, ToolPolicy: c.record.ToolPolicy}}
	record := c.record
	record.Schema = 3
	record.Fork = &origin
	record.Conversation.Session.Slug = "2026-11-18-fork"
	record.Conversation.Session.Incarnation = strings.Repeat("e", 64)
	record.Conversation.NativeID = "44444444-4444-4444-8444-444444444444"
	record.HistoryRef = historyReference(record.Conversation)
	record.HistoryFile = ""
	data, _ := json.Marshal(record)
	if err := strictDecode(data, &decoded, recordLimit); err != nil || validateConversationRecord(decoded, record.Conversation, c.root) != nil {
		t.Fatal("fork variant rejected", err)
	}
	for _, bad := range [][]byte{bytes.Replace(data, []byte(`"Schema":3`), []byte(`"Schema":2`), 1), bytes.Replace(data, []byte(`"Fork":`), []byte(`"fork":`), 1), bytes.Replace(legacy, []byte(`"Schema":2`), []byte(`"Schema":3`), 1)} {
		if strictDecode(bad, &decoded, recordLimit) == nil {
			t.Fatal("invalid variant accepted")
		}
	}
	again, _ := json.Marshal(c.record)
	if !bytes.Equal(legacy, again) {
		t.Fatal("legacy bytes changed")
	}
	request := ForkRequest{Source: h, Snapshot: origin.Snapshot, Destination: CreateRequest{Session: record.Conversation.Session, Member: record.Conversation.Member, Incarnation: record.Conversation.Incarnation, CreationID: record.CreationID, Settings: record.InitialSettings, Instructions: record.Instructions, ToolPolicy: record.ToolPolicy}, NativeID: record.Conversation.NativeID}
	if validateForkRequest(RuntimeRef{Session: record.Conversation.Session}, request) != nil {
		t.Fatal("valid request rejected")
	}
	request.Destination.Session = h.Conversation.Session
	if validateForkRequest(RuntimeRef{Session: h.Conversation.Session}, request) == nil {
		t.Fatal("same session fork accepted")
	}
}

func TestForkNativeExclusionPrecedesLocksAndCopy(t *testing.T) {
	s, c, _, h := policySupervisorFixture(t)
	s.metadata.Protocol = 2
	snapshot := ForkSnapshot{Native: NativeForkSnapshot{SourceFile: filepath.Join(c.root, "history", "source.jsonl"), SourceNativeID: h.Conversation.NativeID, SourceCwd: h.Conversation.Session.Workspace, EntriesDigest: strings.Repeat("d", 64), EntryCount: 2, LeafID: "leaf"}, HistoryRef: c.record.HistoryRef, Settings: c.record.Settings, ToolPolicy: c.record.ToolPolicy}
	session := h.Conversation.Session
	session.Slug = "2026-11-18-destination"
	session.Incarnation = strings.Repeat("e", 64)
	r := ForkRequest{Source: h, Snapshot: snapshot, NativeID: "44444444-4444-4444-8444-444444444444", Destination: CreateRequest{Session: session, Member: h.Conversation.Member, Incarnation: strings.Repeat("f", 64), CreationID: newUUID(), Settings: snapshot.Settings, ToolPolicy: snapshot.ToolPolicy}}
	leave, err := s.hostAdmission.enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.fork(context.Background(), forkOperation{RuntimeRef{Session: session}, r}); !errors.Is(err, ErrHeld) {
		t.Fatal("concurrent effect was not excluded", err)
	}
	leave()
	ctx, release, err := s.hostAdmission.detach(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.hostAdmission.enter(context.Background()); !errors.Is(err, ErrHeld) {
		t.Fatal("new effect entered fork fence", err)
	}
	internal, err := s.hostAdmission.enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	internal()
	release()
	// A quiet RPC counter is insufficient while a sibling still streams or has
	// queued work. Refuse without sending hold/abort and release the fence.
	_, sibling, worker, _ := policySupervisorFixture(t)
	sibling.record.Conversation.Session = h.Conversation.Session
	sibling.record.Generation = h.Generation
	worker.generation = h.Generation
	s.members[sibling.root] = sibling
	target := &forkQuiescentWorker{policyWorker: c.worker.(*policyWorker)}
	c.worker = target
	other := &forkQuiescentWorker{policyWorker: worker}
	sibling.worker = other
	ctx, release, err = s.hostAdmission.detach(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	worker.snapshot.Busy = true
	if err = s.sharedQuiescentLocked(ctx, c, true); !errors.Is(err, ErrHeld) {
		t.Fatal("streaming sibling accepted", err)
	}
	worker.snapshot.Busy = false
	other.queued = true
	if err = s.sharedQuiescentLocked(ctx, c, true); !errors.Is(err, ErrHeld) {
		t.Fatal("queued sibling accepted", err)
	}
	other.queued = false
	if err = s.sharedQuiescentLocked(ctx, c, true); err != nil {
		t.Fatal("quiescent siblings rejected", err)
	}
	c.mu.Unlock()
	release()
	if worker.holds != 0 || target.holds != 0 {
		t.Fatal("fork held sibling")
	}
}

type forkQuiescentWorker struct {
	*policyWorker
	queued bool
}

func (w *forkQuiescentWorker) Call(ctx context.Context, op string, body, out any) error {
	if op == "fork_quiescent_v1" {
		data, _ := json.Marshal(struct {
			Idle bool `json:"idle"`
		}{!w.queued})
		return json.Unmarshal(data, out)
	}
	return w.policyWorker.Call(ctx, op, body, out)
}

func TestPinnedSDKIndependentFork(t *testing.T) {
	m, host, start, ctx := supervisorFixture(t)
	host.metadata.Protocol = 2
	// Freeze the actual current declaration used by this compiled source, rather
	// than advertising support from a test-only legacy manifest.
	contract, err := os.ReadFile("../session/runtime-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var declaration struct {
		PiFork int `json:"piFork"`
	}
	if json.Unmarshal(contract, &declaration) != nil || declaration.PiFork != 1 {
		t.Fatal("current fork contract missing")
	}
	m.config.forkContract = func(string) (bool, error) { return declaration.PiFork == 1, nil }
	source, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	host.s.verifiedInstanceDirectory = m.instance(source.Runtime)
	destHost := &fakeHost{t: t, ctx: ctx, metadata: host.metadata}
	dest := &Manager{config: m.config}
	dest.config.Host = destHost
	destHost.m = dest
	destStart := start
	destStart.Session.Slug = "2026-11-18-fork"
	destStart.Session.Incarnation = strings.Repeat("d", 64)
	destStart.RequestID = "99999999-9999-4999-8999-999999999999"
	if err = os.MkdirAll(filepath.Join(start.Session.Workspace, "work", destStart.Session.Slug), 0700); err != nil {
		t.Fatal(err)
	}
	destination, err := dest.Ensure(ctx, destStart)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if destHost.listener != nil {
			destHost.listener.Close()
		}
		if destHost.s != nil {
			for _, c := range destHost.s.members {
				if c.worker != nil {
					p := c.worker.(*workerProcess)
					select {
					case <-p.host.closed:
					default:
						_ = p.host.command.Process.Kill()
						<-p.host.closed
					}
				}
			}
		}
		destHost.serveWG.Wait()
	})
	for i, member := range []string{"lead", "implementer0"} {
		create := CreateRequest{Session: start.Session, Member: member, Incarnation: strings.Repeat(string(rune('b'+i)), 64), CreationID: []string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"}[i], Settings: settingsFixture()}
		if i == 1 {
			policy := policyTarget("plan", 1, []string{}, []string{})
			create.ToolPolicy = &policy
			create.Instructions = "Retain the saved read-only member role."
		}
		state, err := m.Create(ctx, source.Runtime, create)
		if err != nil {
			t.Fatal(err)
		}
		handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
		ops, err := m.Bind(ctx, handle)
		if err != nil {
			t.Fatal(err)
		}
		sent, err := ops.Send.Send(ctx, BackendIdentity(handle), backend.SendRequest{Message: "source fixture history", ClientUserMessageID: newUUID()})
		if err != nil || sent.Outcome != backend.Accepted {
			t.Fatal(sent, err)
		}
		waitIdle(t, m, ctx, handle)
		frozen, err := m.CaptureFork(ctx, handle)
		if err != nil {
			t.Fatal("capture", err)
		}
		c, _ := host.s.member(handle.Conversation)
		before, err := os.ReadFile(c.record.HistoryFile)
		if err != nil {
			t.Fatal(err)
		}
		request := ForkRequest{Source: handle, Snapshot: frozen, NativeID: []string{"55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666"}[i], Destination: CreateRequest{Session: destStart.Session, Member: member, Incarnation: strings.Repeat(string(rune('e'+i)), 64), CreationID: newUUID(), Settings: frozen.Settings, Instructions: frozen.Instructions, ToolPolicy: frozen.ToolPolicy}}
		if i == 0 {
			held := c.record.HeldReason
			c.record.HeldReason = "fixture Unknown"
			if _, e := m.CaptureFork(ctx, handle); !errors.Is(e, ErrHeld) {
				t.Fatal("held source accepted", e)
			}
			c.record.HeldReason = held
			drift := request
			drift.Snapshot.Native.EntriesDigest = strings.Repeat("f", 64)
			if _, e := host.s.fork(ctx, forkOperation{destination.Runtime, drift}); !errors.Is(e, ErrConflict) {
				t.Fatal("source drift accepted", e)
			}
		}
		// Exercise the source-side lock/revalidation and real destination socket.
		proof, err := host.s.fork(ctx, forkOperation{destination.Runtime, request})
		if err != nil || !proof.Persisted {
			t.Fatal("fork", proof, err)
		}
		h := agentbackend.ExecutionHandle{Conversation: proof.Conversation, Generation: proof.Generation}
		copy, _ := destHost.s.member(h.Conversation)
		if copy.record.WorkerPID == c.record.WorkerPID || copy.receipts.root == c.receipts.root || copy.record.Conversation.NativeID == c.record.Conversation.NativeID {
			t.Fatal("shared source authority")
		}
		if copy.record.Instructions != create.Instructions || !equalJSON(copy.record.ToolPolicy, create.ToolPolicy) {
			t.Fatal("member policy changed")
		}
		entries, _ := os.ReadDir(copy.receipts.root)
		if len(entries) != 0 {
			t.Fatal("inherited receipts")
		}
		if _, err = dest.VerifyCreation(ctx, h, proof.CreationID, proof.HistoryRef, nil); !errors.Is(err, ErrScope) {
			t.Fatal("fork treated as empty bootstrap", err)
		}
		// A lost controller reply reconciles the exact destination, even after
		// the source becomes unavailable. No second native copy or ID is allocated.
		held := c.record.HeldReason
		c.record.HeldReason = "fixture source unavailable"
		recovered, err := dest.Fork(ctx, destination.Runtime, request)
		c.record.HeldReason = held
		if err != nil || recovered.Conversation != proof.Conversation {
			t.Fatal("lost reply recovery", err)
		}
		files, _ := os.ReadDir(filepath.Join(copy.root, "history"))
		if len(files) != 1 {
			t.Fatal("duplicate copy")
		}
		input := SendInput{Message: "fresh destination input"}
		ref := SubmissionRef{Conversation: h.Conversation, OperationID: newUUID(), Action: "send"}
		ref.Digest = SubmissionDigest(ref, input, frozen.Settings)
		accepted, err := dest.SendCheckedSubmission(ctx, h, ref, input)
		if err != nil || accepted.Phase != "Accepted" || accepted.Witness == nil || accepted.Witness.SessionID != h.Conversation.NativeID {
			t.Fatal("independent input", accepted, err)
		}
		waitIdle(t, dest, ctx, h)
		if _, err = dest.VerifyFork(ctx, h, proof.CreationID, proof.Origin); err != nil {
			t.Fatal("prefix after append", err)
		}
		after, _ := os.ReadFile(c.record.HistoryFile)
		if !bytes.Equal(before, after) {
			t.Fatal("source changed")
		}
	}
	var pid int
	for _, c := range destHost.s.members {
		if pid != 0 && pid != c.record.WorkerPID {
			t.Fatal("destination did not reuse shared host")
		}
		pid = c.record.WorkerPID
	}
}
