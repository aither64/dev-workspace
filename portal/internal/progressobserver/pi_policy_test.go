package progressobserver

import (
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestPiObserverRetainsScheduleAndRefusesUnknownOrObserverOnlyWork(t *testing.T) {
	now := time.Now()
	h := agentbackend.ExecutionHandle{Conversation: agentbackend.ConversationRef{Backend: agentbackend.Pi, Member: "implementer0"}, Generation: strings.Repeat("a", 64)}
	activity := PiActivity{Handle: h, State: "working", ObservedAt: now, Known: true}
	state := State{LastCheck: now.Add(-2 * Interval)}
	if due, err := PiDue(state, now, time.Second, []agentbackend.ExecutionHandle{h}, []PiActivity{activity}); !due || err != nil {
		t.Fatalf("%v %v", due, err)
	}
	for _, mutate := range []func(*PiActivity){
		func(a *PiActivity) { a.Observer = true },
		func(a *PiActivity) { a.State = "idle" },
		func(a *PiActivity) { a.Known = false },
		func(a *PiActivity) { a.Held = true },
		func(a *PiActivity) { a.ObservedAt = now.Add(-time.Minute) },
		func(a *PiActivity) { a.Handle.Generation = strings.Repeat("b", 64) },
	} {
		a := activity
		mutate(&a)
		if due, _ := PiDue(state, now, time.Second, []agentbackend.ExecutionHandle{h}, []PiActivity{a}); due {
			t.Fatalf("unproved activity woke observer: %+v", a)
		}
	}
	state.Paused = true
	if due, _ := PiDue(state, now, time.Second, []agentbackend.ExecutionHandle{h}, []PiActivity{activity}); due {
		t.Fatal("bypassed existing pause")
	}
}
