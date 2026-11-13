package communication

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type piCommunicationGate struct {
	scope      agentbackend.SessionRef
	generation string
}

func (g piCommunicationGate) WithPiTeamWrite(ctx context.Context, s agentbackend.SessionRef, generation string, run func() error) error {
	if s != g.scope || generation != g.generation {
		return piruntime.ErrScope
	}
	return run()
}
func TestPiCommunicationUpgradePreservesLegacyPendingAndSamePackageDelivery(t *testing.T) {
	s, ids, transport := fixture(t)
	ctx := context.Background()
	allow(t, s, ids["a"], "b")
	transport.held = "Recipient waiting"
	id := send(t, s, ids["a"], "b", 1)
	before, err := s.Store.read()
	if err != nil {
		t.Fatal(err)
	}
	legacyBytes, _ := json.Marshal(before)
	var legacy state
	if err := json.Unmarshal(legacyBytes, &legacy); err != nil {
		t.Fatal(err)
	}
	same, _ := json.Marshal(&legacy)
	if string(same) != string(legacyBytes) {
		t.Fatal("legacy bytes changed")
	}
	scope := monitoroperation.PiSessionRecord{Workspace: "/workspace", Slug: "2026-11-12-pi", Incarnation: strings.Repeat("a", 64)}
	generation := strings.Repeat("b", 64)
	gate := piCommunicationGate{scope.Ref(), generation}
	pi := EndpointRecord{Pi: &PiEndpoint{Backend: "pi", Session: scope, LeadIncarnation: strings.Repeat("c", 64), NativeID: "pi-lead"}}
	old := ids["a"]
	codex := EndpointRecord{Codex: &old}
	add := func(p *PiCommunicationState) error {
		p.Grants = append(p.Grants, PiGrantRecord{ID: strings.Repeat("d", 32), A: pi, B: codex, ApprovedBy: scope.Slug, Approval: "exact user-approved pair", CreatedAt: time.Now().UTC()})
		return nil
	}
	if err := s.Store.UpdatePi(ctx, nil, scope, generation, add); !errors.Is(err, piruntime.ErrUnavailable) {
		t.Fatal(err)
	}
	still, _ := os.ReadFile(filepath.Join(s.Store.directory, "state.json"))
	if string(still) != string(legacyBytes) {
		t.Fatal("refused upgrade changed legacy bytes")
	}
	if err := s.Store.UpdatePi(ctx, gate, scope, generation, add); err != nil {
		t.Fatal(err)
	}
	after, err := s.Store.read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Schema != 2 || !reflect.DeepEqual(before.Grants, after.Grants) || !reflect.DeepEqual(before.Messages, after.Messages) {
		t.Fatal("upgrade lost legacy pending obligation")
	}
	if err := s.Dispatch(ctx, id); err == nil {
		t.Fatal("schema2 legacy delivery bypassed absent current package gate")
	}
	s.Store.Schema2Write = func(ctx context.Context, run func() error) error {
		return gate.WithPiTeamWrite(ctx, scope.Ref(), generation, run)
	}
	transport.held = ""
	if err := s.Dispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if transport.sends != 1 {
		t.Fatal("legacy message repeated")
	}
	current, err := s.Store.read()
	if err != nil || len(current.Pi.Grants) != 2 || current.Messages[0].Status != "delivered" {
		t.Fatal("same-package legacy delivery erased Pi grant", err)
	}
	data, _ := os.ReadFile(filepath.Join(s.Store.directory, "state.json"))
	var oldReader legacyState
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if d.Decode(&oldReader) == nil && oldReader.Schema == 1 {
		t.Fatal("old reader accepted schema2")
	}
}
func TestPiCommunicationStrictEndpointAndLegacyOptionSeparation(t *testing.T) {
	for _, raw := range []string{`{"backend":"pi","session":{"workspace":"/workspace","slug":"2026-11-12-pi","incarnation":"` + strings.Repeat("a", 64) + `"},"leadIncarnation":"` + strings.Repeat("b", 64) + `","nativeId":"native","thread":"fake-codex"}`, `{"slug":"legacy","thread":"thread","incarnation":"i","backend":"codex"}`, `{"slug":"legacy","thread":null,"incarnation":"i"}`} {
		var endpoint EndpointRecord
		if json.Unmarshal([]byte(raw), &endpoint) == nil {
			t.Fatal("mixed/null endpoint accepted")
		}
	}
}
