package piruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReceiptGenerationRequiresCurrentOrVerifiedAncestor(t *testing.T) {
	for _, fault := range []string{"current", "ancestor", "foreign", "malformed", "missing-startup", "missing-directory", "wrong-config", "wrong-current", "orphan", "missing-history"} {
		t.Run(fault, func(t *testing.T) {
			s, m, records, _, _ := workflowHistoricalResourceFixture(t)
			generation := records.Requests[s.launch.ReplacesRequestID].Runtime.Generation
			directory, _ := stateDirectory(m.config.UserStateRoot, records.Session)
			switch fault {
			case "current":
				generation = s.ref.Generation
			case "foreign":
				generation = strings.Repeat("f", 64)
			case "malformed":
				generation = "not-a-generation"
			case "missing-startup":
				s.reclaimControl = false
			case "missing-directory":
				s.verifiedInstanceDirectory = ""
			case "wrong-config":
				s.handshake.RuntimeConfigSHA256 = strings.Repeat("f", 64)
			case "wrong-current":
				s.ref.RequestID = newUUID()
			case "orphan":
				start := records.Requests[records.CurrentRequestID].Start
				start.RequestID = newUUID()
				orphan, err := m.reserveRuntimeRecord(start, records.InstanceID)
				if err != nil {
					t.Fatal(err)
				}
				records.Requests[start.RequestID] = orphan
				if err := writePrivate(filepath.Join(directory, "runtime.json"), records); err != nil {
					t.Fatal(err)
				}
			case "missing-history":
				if err := os.Remove(filepath.Join(directory, "runtime.json")); err != nil {
					t.Fatal(err)
				}
			}
			err := s.admitReceiptGeneration(generation)
			if (fault == "current" || fault == "ancestor") != (err == nil) {
				t.Fatalf("receipt generation %s: %v", fault, err)
			}
		})
	}
}
