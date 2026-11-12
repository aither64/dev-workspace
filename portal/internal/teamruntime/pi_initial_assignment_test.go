package teamruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"golang.org/x/sys/unix"
	"os"
)

func TestPiFirstAssignmentReservesRealIDAndPublishesOutsideDomainLocks(t *testing.T) {
	for _, originalID := range []string{strings.Repeat("e", 32), "12345678-1234-4234-8234-123456789abc"} {
		t.Run(originalID, func(t *testing.T) { testPiFirstAssignmentOriginalReferences(t, originalID) })
	}
}

func testPiFirstAssignmentOriginalReferences(t *testing.T, originalID string) {
	t.Helper()
	caller, message, _, target := piFixture()
	message.ID = originalID
	workspace := t.TempDir()
	caller.Runtime.Session.Workspace = workspace
	caller.Handle.Conversation.Session.Workspace = workspace
	caller.Handle.Conversation.Member = "lead"
	store, _ := NewStore(t.TempDir(), workspace)
	roster := piRosterFixture(workspace)
	roster.SessionIncarnation = caller.Runtime.Session.Incarnation
	gate := &piWriteFake{session: caller.Runtime.Session, generation: caller.Runtime.Generation}
	if err := store.SavePi(context.Background(), gate, caller.Runtime.Generation, 0, roster); err != nil {
		t.Fatal(err)
	}
	target.Settings.Model = roster.Members[0].Model
	message.To = "implementer0"
	message.Text = "Implement the real assigned package."
	assignment, err := store.ReservePiInitialAssignment(context.Background(), gate, caller, 1, message, target.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.Request.ClientUserMessageID != message.ID || !strings.Contains(assignment.Request.Message, message.Text) || assignment.Request.ActionContext != "team:"+roster.Slug+":lead:implementer0" {
		t.Fatal("invented bootstrap instead of real assignment")
	}
	retry, err := store.ReservePiInitialAssignment(context.Background(), gate, caller, 1, message, target.Settings)
	if err != nil || retry != assignment {
		t.Fatal("initial assignment retry changed reservation", err)
	}
	current, _ := store.LoadPi(roster.Slug)
	if current.Revision != 2 {
		t.Fatal("retry consumed another roster revision")
	}
	handle := agentbackend.ExecutionHandle{Conversation: agentbackend.ConversationRef{Session: assignment.Session, Backend: agentbackend.Pi, Member: assignment.Member, Incarnation: assignment.Incarnation, NativeID: "proved-native"}, Generation: caller.Runtime.Generation}
	if err := assignment.Publish(context.Background(), 2, handle, nil); !errors.Is(err, ErrPiUnavailable) {
		t.Fatal(err)
	}
	for _, creation := range []string{strings.Repeat("d", 32), "12345678-1234-4234-8234-123456789abc", strings.Repeat("D", 64)} {
		invalid := assignment
		invalid.CreationID = creation
		if err := invalid.Publish(context.Background(), 2, handle, func(context.Context, uint64, agentbackend.ExecutionHandle, string, string) error {
			t.Fatal("invalid creation reference reached publication")
			return nil
		}); !errors.Is(err, ErrPiPolicy) {
			t.Fatal(err)
		}
	}
	invalidInput := assignment
	invalidInput.Request.ClientUserMessageID = strings.Repeat("e", 64)
	if err := invalidInput.Publish(context.Background(), 2, handle, func(context.Context, uint64, agentbackend.ExecutionHandle, string, string) error {
		t.Fatal("64hex input ID reached publication")
		return nil
	}); !errors.Is(err, ErrPiPolicy) {
		t.Fatal(err)
	}
	called := false
	err = assignment.Publish(context.Background(), 2, handle, func(ctx context.Context, revision uint64, h agentbackend.ExecutionHandle, creation, id string) error {
		called = true
		if revision != 2 || creation != roster.Members[0].CreationID || id != message.ID {
			t.Fatal("publication changed frozen references")
		}
		lock, err := os.OpenFile(store.operationLockPath(roster.Slug), os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal("publication called inside team ownership")
		}
		return ErrPiHeld // P2 still needs actual native witness.
	})
	if !called || !errors.Is(err, ErrPiHeld) {
		t.Fatal("publication manufactured success", err)
	}
}
