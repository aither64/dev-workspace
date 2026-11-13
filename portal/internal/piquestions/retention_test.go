package piquestions

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeRetainedRecordPreservesEmptyClosedAndEveryPhase(t *testing.T) {
	s, c := fixture(t)
	ref := c.Handle.Conversation
	data, _ := json.Marshal(fresh(ref))
	if r, err := DecodeRetainedRecord(data); err != nil || r.Conversation != ref || r.Questions == nil {
		t.Fatal(r, err)
	}
	q, err := s.Publish(context.Background(), c, PublishInput{Questions: []Question{{Title: "Retained?", Options: []Option{}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AcceptResponse(context.Background(), ref, ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []Answer{{QuestionID: "0", Text: "exact answer"}}})
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
	r.Questions[0].Closed = true
	for _, phase := range []string{"pending", "handoff", "Unknown", "NotSubmitted", "Accepted"} {
		r.Questions[0].Responses[0].Delivery.Phase = phase
		r.Questions[0].Responses[0].Delivery.ReceiptRef = "original-receipt"
		encoded, _ := json.Marshal(r)
		copy, err := DecodeRetainedRecord(encoded)
		if err != nil || !reflect.DeepEqual(copy, r) {
			t.Fatal(phase, err)
		}
		copy.Questions[0].Questions[0].Title = "changed"
		if r.Questions[0].Questions[0].Title != "Retained?" {
			t.Fatal("shared mutable decoded data")
		}
	}
	if _, err = os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("decoder created lock", err)
	}
	for _, bad := range [][]byte{[]byte(`null`), []byte(strings.Replace(string(data), `"schema":1`, `"schema":1,"schema":1`, 1)), []byte(strings.Replace(string(data), `"schema":1`, `"schema":2`, 1)), []byte(strings.Replace(string(data), `"Backend":"pi"`, `"Backend":"codex"`, 1)), []byte(strings.Replace(string(data), `"nextSequence":2`, `"nextSequence":0`, 1)), append(append([]byte{}, data...), []byte(`{}`)...), []byte(strings.Repeat(" ", StoreLimit+1))} {
		if _, err := DecodeRetainedRecord(bad); err == nil {
			t.Fatal("invalid retained record accepted")
		}
	}
}
