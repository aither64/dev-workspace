package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
)

func maintenanceNoticeFixture(t *testing.T) (maintenanceControl, maintenance.Operation, maintenance.Participant, *codex.Client) {
	t.Helper()
	m, _ := maintenanceWorkerFixture(t, false)
	p := maintenance.Participant{Workspace: m.workspace, Slug: "session", Incarnation: "inode", Root: "root", Thread: "thread", Cwd: filepath.Join(m.workspace, "work/session"), SourceTurn: "source-turn", SocketEpoch: "epoch", NoticeID: controlOperationID, NoticeText: "safe boundary", NoticeAction: "maintenance-notice", NoticeDisposition: "refused", ContinuationID: "22223344-1122-4122-8122-112233441122", ContinuationDisposition: "pending"}
	o, e := m.store.Update(context.Background(), controlOperationID, 0, func(o *maintenance.Operation) error {
		o.CutoverIntent = false
		o.Phase = "pausing"
		o.CancelRequested = true
		o.Services = []maintenance.Service{{Workspace: m.workspace, Unit: "unit", Native: "/native", PID: 123, ProcessIdentity: "source", Socket: filepath.Join(m.store.Root, "not-a-live-native.sock"), SocketEpoch: "epoch"}}
		o.Participants = []maintenance.Participant{p}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	c := m.client(o, p)
	t.Cleanup(c.Close)
	if e = c.PrepareSendWithOptions(p.Thread, p.NoticeText, p.NoticeID, maintenanceNoticeContext(p), true, codex.TurnOptions{}); e != nil {
		t.Fatal(e)
	}
	return m, o, p, c
}

type interruptedNoticeDiscard struct {
	maintenanceNoticeClient
	after bool
}

func (c interruptedNoticeDiscard) DiscardPreparedConditionalSteer(ctx context.Context, a, b, d, e, f, g string) error {
	if c.after {
		if err := c.maintenanceNoticeClient.DiscardPreparedConditionalSteer(ctx, a, b, d, e, f, g); err != nil {
			return err
		}
	}
	return errors.New("interrupted retirement receipt")
}
func TestCancellationRetriesNoticeRetirementBeforeAndAfterExactDiscard(t *testing.T) {
	for _, after := range []bool{false, true} {
		m, _, p, c := maintenanceNoticeFixture(t)
		ctx := context.Background()
		if e := m.retireNotice(ctx, ctx, controlOperationID, 0, p, interruptedNoticeDiscard{c, after}); e == nil {
			t.Fatal("interruption was hidden")
		}
		o, e := m.store.Load(controlOperationID)
		if e != nil {
			t.Fatal(e)
		}
		q := o.Participants[0]
		if !q.NoticeRetirementIntent || q.NoticeRetired || q.NoticeDisposition != "refused" {
			t.Fatal(q)
		}
		if e = m.retireNotice(ctx, ctx, controlOperationID, 0, q, c); e != nil {
			t.Fatal(e)
		}
		o, _ = m.store.Load(controlOperationID)
		if !o.Participants[0].NoticeRetired || o.Participants[0].NoticeDisposition != "refused" {
			t.Fatal(o.Participants[0])
		}
		data, e := c.ExportSubmissionAttempts(p.Thread)
		if e != nil {
			t.Fatal(e)
		}
		var row map[string]any
		if json.Unmarshal(data, &row) != nil {
			t.Fatal("invalid export")
		}
		if sends, ok := row["sends"]; ok && sends != nil {
			t.Fatal("prepared notice survived exact retirement")
		}
	}
}
func TestCancelledActionRetiresRefusalWithoutPublishingTerminalState(t *testing.T) {
	m, _, p, c := maintenanceNoticeFixture(t)
	if _, e := m.worker(context.Background(), maintenance.Request{Schema: 1, Action: "cancelled", OperationID: controlOperationID}); e != nil {
		t.Fatal(e)
	}
	o, e := m.store.Load(controlOperationID)
	if e != nil || o.Terminal() || !o.NoticeCleanupComplete || !o.Participants[0].NoticeRetired {
		t.Fatalf("%+v %v", o, e)
	}
	if e = m.retireNotice(context.Background(), context.Background(), controlOperationID, 0, o.Participants[0], c); e != nil {
		t.Fatal("retirement retry", e)
	}
	_ = p
}

type uncertainNoticeClient struct {
	p                    maintenance.Participant
	state                string
	discards, reconciles int
	accepted             bool
}

func (c *uncertainNoticeClient) DiscardPreparedConditionalSteer(context.Context, string, string, string, string, string, string) error {
	c.discards++
	return errors.New("not prepared")
}
func (c *uncertainNoticeClient) ExportSubmissionAttempts(string) (json.RawMessage, error) {
	sum := sha256.Sum256([]byte(c.p.NoticeText))
	return json.Marshal(map[string]any{"schema": 3, "sends": map[string]any{c.p.Thread: map[string]any{c.p.NoticeID: map[string]any{"state": c.state, "context": maintenanceNoticeContext(c.p), "digest": hex.EncodeToString(sum[:]), "steered": true, "turnId": c.p.SourceTurn}}}})
}
func (c *uncertainNoticeClient) ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error) {
	c.reconciles++
	if c.accepted {
		return codex.SendReceipt{TurnID: c.p.SourceTurn, Steered: true, ClientUserMessageID: c.p.NoticeID}, true, nil
	}
	return codex.SendReceipt{}, false, &codex.UnknownSendOutcomeError{Err: errors.New("history unavailable")}
}
func TestCancellationDoesNotErasePreparedJournalSubmittingLedgerRaceOrUnknown(t *testing.T) {
	for _, original := range []string{"prepared", "unknown"} {
		m, _, p, _ := maintenanceNoticeFixture(t)
		p.NoticeDisposition = original
		_, e := m.store.Update(context.Background(), controlOperationID, 0, func(o *maintenance.Operation) error { o.Participants[0].NoticeDisposition = original; return nil })
		if e != nil {
			t.Fatal(e)
		}
		client := &uncertainNoticeClient{p: p, state: "submitting"}
		if e = m.retireNotice(context.Background(), context.Background(), controlOperationID, 0, p, client); e == nil {
			t.Fatal("unknown notice was erased")
		}
		o, _ := m.store.Load(controlOperationID)
		q := o.Participants[0]
		if q.NoticeRetired || !q.NoticeRetirementIntent || q.NoticeDisposition != "unknown" || client.reconciles != 1 {
			t.Fatal(q, client)
		}
		if original == "unknown" && client.discards != 0 {
			t.Fatal("uncertain original was offered for deletion")
		}
		client.accepted = true
		if e = m.retireNotice(context.Background(), context.Background(), controlOperationID, 0, q, client); e != nil {
			t.Fatal(e)
		}
		o, _ = m.store.Load(controlOperationID)
		if o.Participants[0].NoticeDisposition != "accepted" || !o.Participants[0].NoticeCleanupAccepted || o.Participants[0].NoticeRetired {
			t.Fatal(o.Participants[0])
		}
	}
}
