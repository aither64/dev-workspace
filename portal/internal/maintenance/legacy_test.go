package maintenance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func legacyFixture(t *testing.T) (Store, Identity, string, string) {
	t.Helper()
	root := t.TempDir()
	s := Store{Root: root, Profile: filepath.Join(root, "profile")}
	owner := filepath.Join(root, "old-source")
	if err := os.MkdirAll(filepath.Join(owner, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	identity := Identity{Schema: 1, OperationID: testID, Profile: s.Profile, SourcePackage: owner}
	if err := os.MkdirAll(filepath.Dir(s.Path(testID)), 0700); err != nil {
		t.Fatal(err)
	}
	// These obsolete fields are intentionally not understood by the new package.
	data, _ := json.Marshal(map[string]any{"schema": 1, "operationId": testID, "profile": s.Profile, "sourcePackage": owner, "finished": "old-private-value", "deadline": "old-private-value"})
	if err := os.WriteFile(s.Path(testID), data, 0600); err != nil {
		t.Fatal(err)
	}
	pointer, _ := json.Marshal(map[string]any{"schema": 1, "profile": s.Profile, "operationId": testID})
	if err := os.WriteFile(filepath.Join(s.directory(), "active.json"), pointer, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "owner-response")
	requests := filepath.Join(root, "owner-request")
	script := "#!/bin/sh\ncat > " + strconv.Quote(requests) + "\ncat " + strconv.Quote(output) + "\n"
	if err := os.WriteFile(filepath.Join(owner, "bin/workspace-portal"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return s, identity, output, requests
}
func legacyStatus(t *testing.T, s Store, identity Identity, path, phase string) {
	t.Helper()
	body := map[string]any{"schema": 1, "status": phase, "operation": map[string]any{"schema": 1, "operationId": identity.OperationID, "profile": s.Profile, "sourcePackage": identity.SourcePackage, "phase": phase, "services": []any{map[string]any{"workspace": s.Root}}, "participants": []any{map[string]any{"workspace": s.Root, "thread": "actor"}}}}
	data, _ := json.Marshal(body)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestLegacyOwnerStatusControlsAdmissionAndNewCreation(t *testing.T) {
	s, identity, output, requests := legacyFixture(t)
	before, _ := os.ReadFile(s.Path(testID))
	for _, phase := range []string{"awaiting-ready", "future-old-phase"} {
		legacyStatus(t, s, identity, output, phase)
		if err := RequireAdmission(s.Root, s.Profile, s.Root); err == nil {
			t.Fatal("active legacy owner opened admission")
		}
		if err := RequireAdmission(s.Root, s.Profile, s.Root+"/different"); err != nil {
			t.Fatal(err)
		}
	}
	legacyStatus(t, s, identity, output, "complete")
	if err := RequireAdmission(s.Root, s.Profile, s.Root); err != nil {
		t.Fatal(err)
	}
	request, _ := os.ReadFile(requests)
	if !strings.Contains(string(request), `"operationId":"`+testID+`"`) || !strings.Contains(string(request), `"action":"status"`) {
		t.Fatal(string(request))
	}
	o := Operation{Schema: Schema, OperationID: testAck, Revision: 1, Phase: "prepared", Profile: s.Profile, SourceProfileToken: "source", SourcePackage: identity.SourcePackage, CandidatePackage: "/candidate", PlansDigest: "plans", Services: []Service{}, Participants: []Participant{}}
	if err := s.Create(context.Background(), o); err != nil {
		t.Fatal("terminal old pointer blocked new operation", err)
	}
	after, _ := os.ReadFile(s.Path(testID))
	if string(before) != string(after) {
		t.Fatal("old private record rewritten")
	}
}
func TestLegacyStopRoutesOnlyExactCapturedParticipant(t *testing.T) {
	s, identity, output, requests := legacyFixture(t)
	legacyStatus(t, s, identity, output, "pausing")
	// Owner script serves status first, then accepts its own Stop action.
	script := "#!/bin/sh\nr=$(cat)\nprintf '%s' \"$r\" > " + strconv.Quote(requests) + "\ncase \"$r\" in *suppress-resume*) printf '%s' '{\"schema\":1,\"status\":\"suppressed\"}';; *) cat " + strconv.Quote(output) + ";; esac\n"
	if err := os.WriteFile(filepath.Join(identity.SourcePackage, "bin/workspace-portal"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := SuppressActive(context.Background(), s.Root, s.Profile, s.Root, "actor"); err != nil {
		t.Fatal(err)
	}
	request, _ := os.ReadFile(requests)
	if !strings.Contains(string(request), `"action":"suppress-resume"`) || !strings.Contains(string(request), `"threadId":"actor"`) {
		t.Fatal(string(request))
	}
	if err := SuppressActive(context.Background(), s.Root, s.Profile, s.Root+"/other", "actor"); err != nil {
		t.Fatal(err)
	}
	request, _ = os.ReadFile(requests)
	if strings.Contains(string(request), "suppress-resume") {
		t.Fatal("foreign workspace Stop reached owner")
	}
}
func TestLegacyOwnerUnavailabilityAndMalformedStatusHold(t *testing.T) {
	s, identity, output, _ := legacyFixture(t)
	for _, body := range []string{`{}`, `{"schema":1,"status":"complete"}`, `{"schema":1,"schema":1}`, strings.Repeat("x", MaxBytes+1)} {
		if err := os.WriteFile(output, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RequireAdmission(s.Root, s.Profile, s.Root); err == nil {
			t.Fatal("invalid owner status opened fence")
		}
	}
	legacyStatus(t, s, identity, output, "complete")
	if err := os.Remove(filepath.Join(identity.SourcePackage, "bin/workspace-portal")); err != nil {
		t.Fatal(err)
	}
	if err := RequireAdmission(s.Root, s.Profile, s.Root); err == nil {
		t.Fatal("missing owner opened fence")
	}
}

func TestSchema2AdmissionHoldsInactiveWorkspaceInSameProfile(t *testing.T) {
	c, _ := fixture(t)
	if err := RequireAdmission(c.Store.Root, c.Store.Profile, c.Store.Root+"/inactive"); err == nil {
		t.Fatal("inactive workspace escaped profile hold")
	}
	if _, err := c.Store.Update(context.Background(), testID, 0, func(o *Operation) error { o.Phase = "complete"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := RequireAdmission(c.Store.Root, c.Store.Profile, c.Store.Root+"/inactive"); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := RequireAdmission(other, filepath.Join(other, "profile"), other+"/workspace"); err != nil {
		t.Fatal("unrelated profile held", err)
	}
}
