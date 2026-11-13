package piplan

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/piquestions"
)

func TestDecodeRetainedRecordKeepsEmptyOldPlansAndEveryAcceptancePhase(t *testing.T) {
	s, c := fixture(t)
	ref := c.Handle.Conversation
	data, _ := json.Marshal(fresh(ref))
	if r, err := DecodeRetainedRecord(data); err != nil || r.Conversation != ref || r.Proposals == nil || r.Acceptances == nil {
		t.Fatal(r, err)
	}
	p, err := s.PublishProposal(context.Background(), c, "exact retained plan", NativeOrigin{EntryID: "anchor", Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Accept(context.Background(), ref, AcceptanceInput{AcceptanceID: piquestions.UUID(), PlanID: p.PlanID, Revision: p.Revision, TextSHA256: p.TextSHA256})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := s.file(ref).Path()
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	r, err := DecodeRetainedRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"prepared", "mode_pending", "dispatch_pending", "held", "accepted"} {
		r.Acceptances[0].Phase = phase
		encoded, _ := json.Marshal(r)
		copy, err := DecodeRetainedRecord(encoded)
		if err != nil || !reflect.DeepEqual(copy, r) {
			t.Fatal(phase, err)
		}
		copy.Proposals[0].Text = "changed"
		if r.Proposals[0].Text != "exact retained plan" {
			t.Fatal("shared decoded data")
		}
	}
	if _, err = os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("decoder created lock", err)
	}
	for _, bad := range [][]byte{[]byte(`null`), []byte(strings.Replace(string(data), `"schema":1`, `"schema":1,"schema":1`, 1)), []byte(strings.Replace(string(data), `"schema":1`, `"schema":3`, 1)), []byte(strings.Replace(string(data), `"Backend":"pi"`, `"Backend":"codex"`, 1)), []byte(strings.Replace(string(data), `"targetMode":"default"`, `"targetMode":"plan"`, 1)), []byte(strings.Replace(string(data), `"proposals":[`, `"proposals":null,"extra":[`, 1)), []byte(strings.Repeat(" ", piquestions.StoreLimit+1))} {
		if _, err := DecodeRetainedRecord(bad); err == nil {
			t.Fatal("invalid retained plan accepted")
		}
	}
}
