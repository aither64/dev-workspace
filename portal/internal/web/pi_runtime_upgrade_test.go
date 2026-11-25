package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type piFrozenTargetUpgradeFixture struct {
	*piAddedTeamFixture
	current, predecessor piruntime.RuntimeRef
	saved                piruntime.StartRequest
	received             *piruntime.UpgradeRequest
	savedReads           int
	unsupported          bool
}

func (f *piFrozenTargetUpgradeFixture) ReadCurrentRuntime(_ context.Context, ref agentbackend.SessionRef) (piruntime.RuntimeRef, error) {
	if ref != f.current.Session {
		return piruntime.RuntimeRef{}, piruntime.ErrScope
	}
	return f.current, nil
}
func (f *piFrozenTargetUpgradeFixture) ReadStart(context.Context, agentbackend.SessionRef) (piruntime.StartRequest, error) {
	f.savedReads++
	return f.saved, nil
}
func (f *piFrozenTargetUpgradeFixture) ReadInitialStart(ctx context.Context, ref agentbackend.SessionRef, id string) (piruntime.StartRequest, piruntime.RuntimeRef, error) {
	start, err := f.piCreationRuntimeFixture.ReadStart(ctx, ref)
	if err != nil || start.RequestID != id {
		return piruntime.StartRequest{}, piruntime.RuntimeRef{}, piruntime.ErrScope
	}
	return start, f.predecessor, nil
}
func (f *piFrozenTargetUpgradeFixture) Upgrade(_ context.Context, r piruntime.UpgradeRequest, _ piruntime.UpgradeHooks) (piruntime.UpgradeResult, error) {
	f.received = &r
	if r.RecoverTeamConflict == nil || r.Start != f.saved || r.Start.ReplacesRequestID != f.predecessor.RequestID || r.ExpectedGeneration != f.predecessor.Generation {
		return piruntime.UpgradeResult{}, piruntime.ErrScope
	}
	if f.unsupported {
		return piruntime.UpgradeResult{}, piruntime.ErrUnavailable
	}
	return piruntime.UpgradeResult{Runtime: f.current, Previous: f.predecessor, AppStorePath: f.saved.AppStorePath, Phase: "ready", Disposition: "recovered-team-conflict", TeamMessageID: r.RecoverTeamConflict.OperationID}, nil
}

func TestPiRuntimeRecoveryReaderPreservesFrozenTarget(t *testing.T) {
	path := os.Getenv("PI_START_RECOVERY_PACKAGES")
	if path == "" {
		t.Skip("explicit immutable package metadata A/B inputs required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var packages struct{ Roots map[string]string }
	if err := json.Unmarshal(raw, &packages); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"", "session", "incarnation", "binding", "profile-path", "missing-predecessor", "predecessor", "generation", "saved-capability", "stale-reader", "ordinary-target", "ordinary-profile", "retirement-target", "retirement-profile"} {
		t.Run(fault, func(t *testing.T) {
			d, ports, _, _ := conflictDomainFixture(t)
			s := d.server
			// Existing immutable A/B metadata input: current selection is B, while
			// the saved execution target remains A. No SDK or host is executed.
			s.config.PackageRoot = packages.Roots["b"]
			if err := os.Remove(s.config.HostProfile); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(s.config.PackageRoot, s.config.HostProfile); err != nil {
				t.Fatal(err)
			}
			s.hostProfile, err = readHostProfileIdentity(s.config.HostProfile)
			if err != nil {
				t.Fatal(err)
			}
			f := &piFrozenTargetUpgradeFixture{piAddedTeamFixture: ports, predecessor: ports.runtime, current: ports.runtime}
			f.saved = piruntime.StartRequest{Session: f.current.Session, BindingRevision: d.current.Binding.Revision,
				RequestID: "77777777-7777-4777-8777-777777777777", ReplacesRequestID: f.predecessor.RequestID,
				AppStorePath: packages.Roots["a"], ProfilePath: s.config.HostProfile, ProfileGeneration: "original-execution-profile"}
			f.current.RequestID, f.current.Generation = f.saved.RequestID, strings.Repeat("f", 64)
			s.config.PiCreation.Runtime = f
			_, release, admissionErr := s.acquirePiSessionAdmissionFor(context.Background(), f.current.Session, true, true)
			if admissionErr != nil {
				t.Fatal("owning upgrade fixture admission", admissionErr)
			}
			release()
			if err := validatePiPackage(s.config, s.hostProfile); err != nil {
				t.Fatal("owning selected reader fixture", err)
			}
			input := map[string]string{"workspace": s.config.Workspace, "requestId": f.saved.RequestID,
				"expectedGeneration": f.predecessor.Generation, "recoverTeamConflict": "55555555-5555-4555-8555-555555555555", "member": "lead"}
			backendRefusal := false
			switch fault {
			case "session":
				f.saved.Session.Slug += "-other"
			case "incarnation":
				f.saved.Session.Incarnation = "88888888-8888-4888-8888-888888888888"
			case "binding":
				f.saved.BindingRevision++
			case "profile-path":
				f.saved.ProfilePath += "-other"
			case "missing-predecessor":
				f.saved.ReplacesRequestID = ""
			case "predecessor":
				f.saved.ReplacesRequestID = "99999999-9999-4999-8999-999999999999"
				backendRefusal = true
			case "generation":
				input["expectedGeneration"] = strings.Repeat("e", 64)
				backendRefusal = true
			case "saved-capability":
				// The Manager owns actual capability inspection; the route must
				// preserve its refusal rather than substituting the reader target.
				f.unsupported, backendRefusal = true, true
			case "stale-reader":
				if err := os.Remove(s.config.HostProfile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(packages.Roots["a"], s.config.HostProfile); err != nil {
					t.Fatal(err)
				}
			case "ordinary-target", "ordinary-profile", "retirement-target", "retirement-profile":
				delete(input, "recoverTeamConflict")
				delete(input, "member")
				if strings.HasSuffix(fault, "profile") {
					f.saved.AppStorePath = s.config.PackageRoot
				}
				if strings.HasPrefix(fault, "retirement") {
					input["retireUnknownInput"] = "55555555-5555-4555-8555-555555555555"
					input["inputDigest"], input["member"] = strings.Repeat("b", 64), "lead"
				}
			}
			body, _ := json.Marshal(input)
			request := httptest.NewRequest(http.MethodPost, s.config.BaseURL+"/api/sessions/"+f.current.Session.Slug+"/pi-runtime-upgrade", strings.NewReader(string(body)))
			request.Header.Set("Origin", s.config.BaseURL)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			s.sessionAPI(response, request)
			if fault != "stale-reader" && f.savedReads != 1 {
				t.Fatal("request did not reach owning frozen-Start boundary", f.savedReads)
			}
			if fault != "" {
				if response.Code == http.StatusOK || (f.received != nil) != backendRefusal {
					t.Fatal("invalid continuation admitted or wrong refusal boundary", response.Code, response.Body.String(), f.received)
				}
				return
			}
			if response.Code != http.StatusOK || f.received == nil || f.received.Start != f.saved {
				t.Fatal("new reader did not pass the complete frozen Start unchanged", response.Code, response.Body.String())
			}
			var result piruntime.UpgradeResult
			if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.AppStorePath != f.saved.AppStorePath || result.Runtime != f.current {
				t.Fatal("reply retargeted the operation", response.Body.String())
			}
		})
	}

}

func TestPiRuntimeUpgradeRouteRefusesClientAuthorityBeforeRuntime(t *testing.T) {
	s := newTestServer(t)
	data, err := json.Marshal(map[string]string{"workspace": s.config.Workspace,
		"requestId": "77777777-7777-4777-8777-777777777777", "expectedGeneration": strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, tc := range []struct {
		name, method, origin, contentType, query, body string
		status                                         int
	}{
		{"read", "GET", s.config.BaseURL, "application/json", "", body, http.StatusMethodNotAllowed},
		{"missing-origin", "POST", "", "application/json", "", body, http.StatusForbidden},
		{"foreign-origin", "POST", "https://foreign.invalid", "application/json", "", body, http.StatusForbidden},
		{"media", "POST", s.config.BaseURL, "text/plain", "", body, http.StatusBadRequest},
		{"target", "POST", s.config.BaseURL, "application/json", "", strings.TrimSuffix(body, "}") + `,"appStorePath":"/client-target"}`, http.StatusBadRequest},
		{"duplicate", "POST", s.config.BaseURL, "application/json", "", strings.TrimSuffix(body, "}") + `,"requestId":"77777777-7777-4777-8777-777777777777"}`, http.StatusBadRequest},
		{"compact-uuid", "POST", s.config.BaseURL, "application/json", "", strings.ReplaceAll(body, "77777777-7777-4777-8777-777777777777", "77777777777747778777777777777777"), http.StatusBadRequest},
		{"generation", "POST", s.config.BaseURL, "application/json", "", strings.ReplaceAll(body, strings.Repeat("a", 64), "unknown"), http.StatusBadRequest},
		{"workspace", "POST", s.config.BaseURL, "application/json", "", strings.ReplaceAll(body, s.config.Workspace, "/foreign"), http.StatusBadRequest},
		{"query", "POST", s.config.BaseURL, "application/json", "?target=other", body, http.StatusBadRequest},
		{"partial-retirement", "POST", s.config.BaseURL, "application/json", "", strings.TrimSuffix(body, "}") + `,"retireUnknownInput":"77777777-7777-4777-8777-777777777777"}`, http.StatusBadRequest},
		{"wrong-retirement-member", "POST", s.config.BaseURL, "application/json", "", strings.TrimSuffix(body, "}") + `,"retireUnknownInput":"77777777-7777-4777-8777-777777777777","inputDigest":"` + strings.Repeat("b", 64) + `","member":"../other"}`, http.StatusBadRequest},
		{"missing-recovery-member", "POST", s.config.BaseURL, "application/json", "", strings.TrimSuffix(body, "}") + `,"recoverTeamConflict":"77777777777747778777777777777777"}`, http.StatusBadRequest},
		{"mixed-recovery", "POST", s.config.BaseURL, "application/json", "", strings.TrimSuffix(body, "}") + `,"recoverTeamConflict":"77777777777747778777777777777777","member":"lead","retireUnknownInput":"77777777-7777-4777-8777-777777777777","inputDigest":"` + strings.Repeat("b", 64) + `"}`, http.StatusBadRequest},
		{"recovery-digest", "POST", s.config.BaseURL, "application/json", "", strings.TrimSuffix(body, "}") + `,"recoverTeamConflict":"77777777777747778777777777777777","member":"lead","inputDigest":"` + strings.Repeat("b", 64) + `"}`, http.StatusBadRequest},
		{"invalid-recovery-id", "POST", s.config.BaseURL, "application/json", "", strings.TrimSuffix(body, "}") + `,"recoverTeamConflict":"UPPERCASE","member":"lead"}`, http.StatusBadRequest},
		{"unavailable-runtime", "POST", s.config.BaseURL, "application/json", "", body, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, s.config.BaseURL+"/api/sessions/example/pi-runtime-upgrade"+tc.query, strings.NewReader(tc.body))
			request.Header.Set("Origin", tc.origin)
			request.Header.Set("Content-Type", tc.contentType)
			response := httptest.NewRecorder()
			s.sessionAPI(response, request)
			if response.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
		})
	}
}
