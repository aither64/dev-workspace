package progressobserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type piObserverGate struct {
	scope      agentbackend.SessionRef
	generation string
}

func (g piObserverGate) WithPiTeamWrite(ctx context.Context, s agentbackend.SessionRef, generation string, run func() error) error {
	if s != g.scope || generation != g.generation {
		return piruntime.ErrScope
	}
	return run()
}
func TestPiObserverCodecAdmissionAndLegacyRootSeparation(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root, "/workspace")
	scope := monitoroperation.PiSessionRecord{Workspace: "/workspace", Slug: "2026-11-12-fixture", Incarnation: strings.Repeat("a", 64)}
	generation := strings.Repeat("b", 64)
	gate := piObserverGate{scope.Ref(), generation}
	state := PiState{Schema: 2, Backend: "pi", Session: scope, LeadIncarnation: strings.Repeat("c", 64), LastCheck: time.Now().UTC()}
	data, _ := json.Marshal(state)
	if _, err := DecodePiState(data); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePi(context.Background(), nil, generation, state); !errors.Is(err, piruntime.ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing admission created observer state")
	}
	if err := store.SavePi(context.Background(), gate, generation, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadPi(scope, state.LeadIncarnation)
	if err != nil || loaded.Session != scope {
		t.Fatal(err)
	}
	if err := store.Save(State{Schema: 1, Root: state.key()}); err == nil {
		t.Fatal("legacy writer replaced Pi state")
	}
	if _, err := store.Load(state.key()); err == nil {
		t.Fatal("legacy reader accepted Pi state")
	}
	for _, bad := range []string{strings.Replace(string(data), `"schema":2`, `"schema":2,"schema":2`, 1), strings.Replace(string(data), `"backend":"pi"`, `"backend":"pi","root":"codex-placeholder"`, 1), string(data) + `{}`} {
		if _, err := DecodePiState([]byte(bad)); err == nil {
			t.Fatal("ambiguous observer accepted")
		}
	}
}
