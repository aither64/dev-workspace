package piquestions

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func fixture(t *testing.T) (*FileStore, Caller) {
	t.Helper()
	ref := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: t.TempDir(), Slug: "2026-11-11-example", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "native"}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return &FileStore{Root: root, Validate: func(context.Context, agentbackend.ConversationRef, *Caller) error { return nil }}, Caller{Handle: agentbackend.ExecutionHandle{Conversation: ref, Generation: "generation"}, Lineage: LineageRef{"anchor", "branch"}, NativeToolCallID: "call"}
}
func TestCaptureAndConcurrentCorrections(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	q, err := s.Publish(ctx, c, PublishInput{[]Question{{Title: "Which?", Options: []Option{{Label: "One"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Publish(ctx, c, PublishInput{q.Questions})
	if err != nil || again.ID != q.ID {
		t.Fatal(again, err)
	}
	changed := c
	changed.Handle.Generation = "restarted"
	again, err = s.Publish(ctx, changed, PublishInput{q.Questions})
	if err != nil || again.ID != q.ID {
		t.Fatal("restart", err)
	}
	_, err = s.Publish(ctx, c, PublishInput{[]Question{{Title: "Changed"}}})
	if !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	in := ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []Answer{{QuestionID: "0", ChoiceID: "0", Text: "details"}}}
	a, err := s.AcceptResponse(ctx, c.Handle.Conversation, in)
	if err != nil || a.Status != "pending" {
		t.Fatal(a, err)
	}
	againA, err := s.AcceptResponse(ctx, c.Handle.Conversation, in)
	if err != nil || againA.Sequence != a.Sequence {
		t.Fatal(againA, err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			correction := in
			correction.ResponseID = UUID()
			correction.SupersedesResponseID = in.ResponseID
			_, err := s.AcceptResponse(ctx, c.Handle.Conversation, correction)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok, conflicts := 0, 0
	for err := range results {
		if err == nil {
			ok++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Fatal(ok, conflicts)
	}
	snap, err := s.Snapshot(ctx, c.Handle.Conversation)
	if err != nil || snap.Questions[0].LatestResponse.Sequence != 2 {
		t.Fatal(snap, err)
	}
}

func TestStrictStateAndScope(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	if _, err := s.Snapshot(ctx, c.Handle.Conversation); err != nil {
		t.Fatal(err)
	}
	path, err := s.file(c.Handle.Conversation).Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read wrote state", err)
	}
	q, err := s.Publish(ctx, c, PublishInput{[]Question{{Title: "Choice", Options: []Option{{Label: "One"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`{"schema":1,"schema":1}`, `{"schema":null}`, `{"Schema":1}`, `{"unexpected":1}`} {
		var record Record
		if Decode([]byte(field), &record) == nil {
			t.Fatal("accepted", field)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Snapshot(ctx, c.Handle.Conversation); err == nil {
		t.Fatal("public permissions accepted")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(data[:len(data)-2], []byte(`,"unknown":true}`)...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Snapshot(ctx, c.Handle.Conversation); err == nil {
		t.Fatal("unknown field accepted")
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	s.Validate = func(_ context.Context, _ agentbackend.ConversationRef, caller *Caller) error {
		if caller != nil {
			return ErrConflict
		}
		return nil
	}
	snap, err := s.Snapshot(ctx, c.Handle.Conversation)
	if err != nil || snap.Questions[0].Status != "stale" || snap.Questions[0].CanRespond {
		t.Fatal(snap, err)
	}
	_, err = s.AcceptResponse(ctx, c.Handle.Conversation, ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []Answer{{QuestionID: "0", Text: "late"}}})
	if !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestCapacityRefusesWithoutPublishing(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	f := s.file(c.Handle.Conversation)
	huge := strings.Repeat("a", StoreLimit+1)
	err := f.Update(ctx, &huge, func() error { return nil })
	if !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	path, _ := f.Path()
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("oversize record published", err)
	}
}

func TestPartialAnswerProjectionPreservesOtherItems(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	q, err := s.Publish(ctx, c, PublishInput{[]Question{{Title: "First"}, {Title: "Second"}}})
	if err != nil {
		t.Fatal(err)
	}
	previous := ""
	for _, answer := range []Answer{{QuestionID: "0", Text: "first answer"}, {QuestionID: "1", Text: "second answer"}, {QuestionID: "0", Text: "corrected first"}} {
		in := ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: 1, SupersedesResponseID: previous, Answers: []Answer{answer}}
		if _, err = s.AcceptResponse(ctx, c.Handle.Conversation, in); err != nil {
			t.Fatal(err)
		}
		previous = in.ResponseID
	}
	snap, err := s.Snapshot(ctx, c.Handle.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	answers := snap.Questions[0].Answers
	if len(answers) != 2 || answers["0"].Text != "corrected first" || answers["1"].Text != "second answer" {
		t.Fatal(answers)
	}
	in := ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: 1, SupersedesResponseID: previous, Answers: []Answer{{QuestionID: "1", Text: "a"}, {QuestionID: "1", Text: "b"}}}
	if _, err = s.AcceptResponse(ctx, c.Handle.Conversation, in); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
