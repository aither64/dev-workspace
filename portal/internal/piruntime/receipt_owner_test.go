package piruntime

import (
	"context"
	"errors"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetainedReceiptV1ExactBytesUnchanged(t *testing.T) {
	engine, ref, input, settings := receiptFixture(t)
	if ref.Digest != "b8ca521ead4ea4948ea981ff327cd09d7e543d50a97bdbc486471805fcb5861c" {
		t.Fatal("v1 immutable digest changed", ref.Digest)
	}
	when := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	status := SubmissionStatus{Schema: 1, Ref: ref, Phase: "NotSubmitted", Generation: "original-generation", Input: input, Settings: settings, Refusal: "before native", CreatedAt: when, UpdatedAt: when}
	if err := engine.save(status); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(engine.root, ref.OperationID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"Schema":1,"Ref":{"Conversation":{"Session":{"Workspace":"/tmp","Slug":"2026-11-12-fixture","Incarnation":"session"},"Backend":"pi","Member":"lead","Incarnation":"member","NativeID":"native-session"},"OperationID":"11111111-1111-4111-8111-111111111111","Action":"send","Digest":"b8ca521ead4ea4948ea981ff327cd09d7e543d50a97bdbc486471805fcb5861c"},"Phase":"NotSubmitted","Generation":"original-generation","Settings":{"Provider":"fake","AccountRef":"","Model":"model","Thinking":"","PolicyRevision":"","Revision":1},"Input":{"Message":"identical text","ActionContext":"","Retry":false},"Witness":null,"Refusal":"before native","CreatedAt":"2026-11-12T00:00:00Z","UpdatedAt":"2026-11-12T00:00:00Z"}`
	if string(data) != want+"\n" {
		t.Fatal("schema1 bytes changed", string(data))
	}
}
func utilityReceiptFixture(t *testing.T) (*receiptEngine, UtilitySubmissionRef, SendInput, EffectiveSettings) {
	t.Helper()
	ref := UtilitySubmissionRef{Utility: UtilityRef{Session: agentbackend.SessionRef{Workspace: "/espace/é", Slug: "2026-11-11-fixture", Incarnation: strings.Repeat("a", 64)}, OperationID: "11111111-1111-4111-8111-111111111111", Incarnation: strings.Repeat("b", 64), NativeID: "22222222-2222-4222-8222-222222222222"}, OperationID: "33333333-3333-4333-8333-333333333333", Action: "send"}
	input := SendInput{Message: "original monitor brief", ActionContext: "monitor-launch:" + ref.Utility.OperationID}
	settings := settingsFixture()
	ref.Digest = UtilitySubmissionDigest(ref, input, settings)
	return &receiptEngine{root: filepath.Join(t.TempDir(), "submissions"), ownerKind: "utility"}, ref, input, settings
}
func TestUtilityUsesOneReceiptEngineAndRejectsWrongOwnerRoot(t *testing.T) {
	for _, point := range []string{"intent", "dispatch", "native", "witness", "receipt"} {
		t.Run(point, func(t *testing.T) {
			engine, ref, input, settings := utilityReceiptFixture(t)
			calls := 0
			engine.checkpoint = func(at string) error {
				if at == point {
					return errors.New("finite loss")
				}
				return nil
			}
			dispatch := func(context.Context) (nativeResult, error) {
				calls++
				return nativeResult{Outcome: "Accepted", Witness: &workerWitness{SessionID: ref.Utility.NativeID, EntryID: "entry", EffectiveMessage: input.Message}}, nil
			}
			if _, err := engine.sendUtility(context.Background(), ref, input, settings, "old", dispatch, func() {}); err == nil {
				t.Fatal("missing loss")
			}
			reopened := &receiptEngine{root: engine.root, ownerKind: "utility"}
			status, err := reopened.lookupUtility(ref)
			if err != nil {
				t.Fatal(err)
			}
			want := "Unknown"
			if point == "receipt" {
				want = "Accepted"
			}
			if status.Phase != want || status.Schema != 2 || status.OwnerKind != "utility" {
				t.Fatal(status)
			}
			before := calls
			if _, err := reopened.sendUtility(context.Background(), ref, input, settings, "new", dispatch, func() {}); err != nil || calls != before {
				t.Fatal("utility replayed", err)
			}
			if _, err := (&receiptEngine{root: engine.root}).load(ref.OperationID); !errors.Is(err, ErrConflict) {
				t.Fatal("utility decoded as member", err)
			}
			if _, err := reopened.load(ref.OperationID); !errors.Is(err, ErrConflict) {
				t.Fatal("retained view converted utility", err)
			}
			changed := input
			changed.Message = "changed"
			conflict := ref
			conflict.Digest = UtilitySubmissionDigest(conflict, changed, settings)
			if _, err := reopened.sendUtility(context.Background(), conflict, changed, settings, "new", dispatch, func() {}); !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
		})
	}
	retained, ref, _, _ := receiptFixture(t)
	when := time.Now().UTC()
	if err := retained.save(SubmissionStatus{Schema: 1, Ref: ref, Phase: "NotSubmitted", Input: SendInput{Message: "identical text"}, Settings: EffectiveSettings{Provider: "fake", Model: "model", Revision: 1}, Refusal: "before native", CreatedAt: when, UpdatedAt: when}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&receiptEngine{root: retained.root, ownerKind: "utility"}).loadOwned(ref.OperationID); !errors.Is(err, ErrConflict) {
		t.Fatal("member receipt converted into utility", err)
	}
}
