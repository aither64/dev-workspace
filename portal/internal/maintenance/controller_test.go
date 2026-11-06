package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const testID = "11111111-1111-4111-8111-111111111111"
const testAck = "22222222-2222-4222-8222-222222222222"

func fixture(t *testing.T) (Controller, Request) {
	t.Helper()
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	workspace := filepath.Join(root, "workspace")
	c := Controller{Store: Store{Root: root, Profile: profile}}
	o := Operation{Schema: Schema, OperationID: testID, Revision: 1, Phase: "pausing", Profile: profile, SourceProfileToken: "source", SourcePackage: "/source", CandidatePackage: "/candidate", PlansDigest: "plans", Services: []Service{{Workspace: workspace, Unit: "workspace-codex@test.service", Native: "/native", PID: 123, ProcessIdentity: "123:1", Socket: "/socket", SocketEpoch: "epoch"}}, Participants: []Participant{{Workspace: workspace, Slug: "session", Incarnation: "inode", Root: "root", Thread: "thread", Cwd: filepath.Join(workspace, "work/session"), SourceTurn: "turn", SocketEpoch: "epoch", NoticeID: testID, NoticeText: "maintenance", NoticeAction: "notice", NoticeDisposition: "accepted", ContinuationID: testAck, ContinuationDisposition: "pending"}}, Blockers: []string{}}
	if err := c.Store.Create(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	return c, Request{Schema: 1, Action: "status", OperationID: testID, ThreadID: "thread"}
}

func TestCancelBeforeAndAfterIrreversibleIntent(t *testing.T) {
	c, _ := fixture(t)
	if r, e := c.Cancel(context.Background(), testID); e != nil || r.Status != "cancelled" {
		t.Fatalf("%+v %v", r, e)
	}
	_, e := c.Store.Update(context.Background(), testID, 0, func(o *Operation) error {
		o.CancelRequested = false
		o.CutoverIntent = true
		o.Phase = "stopping-native"
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if r, e := c.Cancel(context.Background(), testID); e != nil || r.Status != "too-late" {
		t.Fatalf("%+v %v", r, e)
	}
}
func TestPrivateStatusDoesNotDiscloseNoticeInputs(t *testing.T) {
	c, _ := fixture(t)
	r, e := c.Status(testID)
	if e != nil {
		t.Fatal(e)
	}
	p := r.Operation.Participants[0]
	if p.NoticeText != "" || p.NoticeAction != "" {
		t.Fatal("status leaked private notice")
	}
	o, e := c.Store.Load(testID)
	if e != nil || o.Participants[0].NoticeText == "" {
		t.Fatal("status mutated journal")
	}
}
func TestStoreRejectsCorruptionAndStaleRevision(t *testing.T) {
	c, _ := fixture(t)
	o, _ := c.Store.Load(testID)
	if _, e := c.Store.Update(context.Background(), testID, o.Revision+1, func(*Operation) error { return nil }); e == nil {
		t.Fatal("stale update accepted")
	}
	if e := os.WriteFile(c.Store.Path(testID), []byte(`{"schema":1,"schema":1}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Store.Load(testID); e == nil {
		t.Fatal("duplicate field accepted")
	}
}
func TestStoreSerializesConcurrentStopIntent(t *testing.T) {
	c, r := fixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := c.SuppressResume(context.Background(), r); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	o, _ := c.Store.Load(testID)
	if o.Participants[0].StopRevision != 8 {
		t.Fatal(o.Participants[0].StopRevision)
	}
}

func TestContinuationAdmissionLinearizesStop(t *testing.T) {
	for _, stopBefore := range []bool{true, false} {
		t.Run(map[bool]string{true: "before", false: "after"}[stopBefore], func(t *testing.T) {
			c, r := fixture(t)
			ctx := context.Background()
			_, e := c.Store.Update(ctx, testID, 0, func(o *Operation) error {
				o.Phase = "resuming"
				o.CancelRequested = true
				return nil
			})
			if e != nil {
				t.Fatal(e)
			}
			if stopBefore {
				if _, e = c.SuppressResume(ctx, r); e != nil {
					t.Fatal(e)
				}
			}
			result, e := c.AdmitContinuation(ctx, testID, "thread", "inode")
			if e != nil {
				t.Fatal(e)
			}
			if stopBefore && result.Status != "suppressed" || !stopBefore && result.Status != "admitted" {
				t.Fatal(result)
			}
			if !stopBefore {
				if _, e = c.SuppressResume(ctx, r); e != nil {
					t.Fatal(e)
				}
			}
			o, _ := c.Store.Load(testID)
			p := o.Participants[0]
			expected := "suppressed"
			if !stopBefore {
				expected = "submitting"
			}
			if p.ContinuationDisposition != expected {
				t.Fatal("Stop intent or admitted uncertainty lost")
			}
		})
	}
}

func TestMissingActivePointerFencesUntilExactCreateRetry(t *testing.T) {
	c, _ := fixture(t)
	o, e := c.Store.Load(testID)
	if e != nil {
		t.Fatal(e)
	}
	// Original immutable create identity is retained while ordinary status changes.
	original := o
	original.CreateDigest = ""
	if e = os.Remove(filepath.Join(c.Store.directory(), "active.json")); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Store.Active(); e == nil || errors.Is(e, os.ErrNotExist) {
		t.Fatal("orphan operation treated as absent")
	}
	if e = c.Store.Create(context.Background(), original); e != nil {
		t.Fatal(e)
	}
	if active, e := c.Store.Active(); e != nil || active.OperationID != testID {
		t.Fatalf("%+v %v", active, e)
	}
	changed := original
	changed.SourcePackage = "/other"
	if e = c.Store.Create(context.Background(), changed); e == nil {
		t.Fatal("create accepted changed source")
	}
}
