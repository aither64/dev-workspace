package piruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func retentionFixture(t *testing.T) (string, string, string, conversationRecord, UtilitySubmissionStatus) {
	t.Helper()
	state, workspace := t.TempDir(), t.TempDir()
	_, ref, input, settings := utilityReceiptFixture(t)
	ref.Utility.Session.Workspace = workspace
	ref.Digest = UtilitySubmissionDigest(ref, input, settings)
	directory, err := stateDirectory(state, ref.Utility.Session)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(directory, "utilities", ref.Utility.OperationID, ref.Utility.Incarnation)
	mapping := conversationRecord{Schema: 2, OwnerKind: "utility", Utility: &ref.Utility, CreationID: strings.Repeat("f", 32), Generation: strings.Repeat("c", 64), Settings: settings, InitialSettings: settings, HistoryFile: filepath.Join(root, "history", "native.jsonl")}
	if err := writePrivate(filepath.Join(root, "conversation.json"), mapping); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	status := UtilitySubmissionStatus{Schema: 2, OwnerKind: "utility", Ref: ref, Phase: "Unknown", Generation: strings.Repeat("d", 64), Settings: settings, Input: input, CreatedAt: when, UpdatedAt: when}
	if err := (&receiptEngine{root: filepath.Join(root, "submissions"), ownerKind: "utility"}).saveOwned(fromUtility(status)); err != nil {
		t.Fatal(err)
	}
	return state, workspace, root, mapping, status
}
func privateInventory(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			result[path] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		if err == nil {
			result[path] = string(data)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestInspectRetainedUtilitiesIsPureAndKeepsEveryOutcome(t *testing.T) {
	state, workspace, root, mapping, status := retentionFixture(t)
	engine := receiptEngine{root: filepath.Join(root, "submissions"), ownerKind: "utility"}
	for i, phase := range []string{"prepared", "dispatching", "Accepted", "NotSubmitted"} {
		next := status
		next.Phase = phase
		next.Ref.OperationID = fmt.Sprintf("%032x", i+1)
		next.Ref.Digest = UtilitySubmissionDigest(next.Ref, next.Input, next.Settings)
		if phase == "Accepted" {
			next.Witness = &NativeWitness{SessionID: next.Ref.Utility.NativeID, EntryID: "native-entry", EffectiveDigest: MessageDigest(next.Input.Message)}
		}
		if phase == "NotSubmitted" {
			next.Refusal = "positive pre-dispatch refusal"
		}
		if err := engine.saveOwned(fromUtility(next)); err != nil {
			t.Fatal(err)
		}
	}
	if err := makePrivateDirectory(filepath.Join(root, "history")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mapping.HistoryFile, []byte("opaque history is not parsed by preflight"), 0600); err != nil {
		t.Fatal(err)
	}
	before := privateInventory(t, state)
	result, err := InspectRetainedUtilities(context.Background(), state, []string{workspace})
	if err != nil || len(result) != 1 || result[0].Utility != *mapping.Utility || result[0].Generation != mapping.Generation || result[0].CreationID != mapping.CreationID || result[0].Settings != mapping.Settings || len(result[0].Receipts) != 5 {
		t.Fatal(result, err)
	}
	phases := map[string]bool{}
	for _, receipt := range result[0].Receipts {
		phases[receipt.Phase] = true
		if receipt.Generation != status.Generation || receipt.Ref.Utility != *mapping.Utility {
			t.Fatal("original receipt evidence changed", receipt.Generation)
		}
	}
	for _, phase := range []string{"prepared", "dispatching", "Accepted", "NotSubmitted", "Unknown"} {
		if !phases[phase] {
			t.Fatal("missing retained phase", phase)
		}
	}
	if after := privateInventory(t, state); !reflect.DeepEqual(before, after) {
		t.Fatal("observational inventory mutated state")
	}
	empty := t.TempDir()
	if result, err := InspectRetainedUtilities(context.Background(), empty, []string{workspace}); err != nil || len(result) != 0 {
		t.Fatal(result, err)
	}
	if _, err := os.Stat(filepath.Join(empty, "pi-runtime")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("scanner created runtime directory", err)
	}
}
func TestInspectRetainedUtilitiesRefusesOrphanScopeOwnerAndGeneration(t *testing.T) {
	for _, kind := range []string{"mapping-missing", "mapping-scope", "receipt-owner", "receipt-ref", "receipt-generation", "receipt-digest", "receipt-filename", "unknown-workspace", "artifact"} {
		t.Run(kind, func(t *testing.T) {
			state, workspace, root, mapping, status := retentionFixture(t)
			receiptPath := filepath.Join(root, "submissions", status.Ref.OperationID+".json")
			switch kind {
			case "mapping-missing":
				if err := os.Remove(filepath.Join(root, "conversation.json")); err != nil {
					t.Fatal(err)
				}
			case "mapping-scope":
				mapping.Utility.Incarnation = strings.Repeat("e", 64)
				if err := writePrivate(filepath.Join(root, "conversation.json"), mapping); err != nil {
					t.Fatal(err)
				}
			case "receipt-owner":
				status.OwnerKind = "retained"
				if err := writePrivate(receiptPath, status); err != nil {
					t.Fatal(err)
				}
			case "receipt-ref":
				status.Ref.Utility.NativeID = "44444444-4444-4444-8444-444444444444"
				status.Ref.Digest = UtilitySubmissionDigest(status.Ref, status.Input, status.Settings)
				if err := writePrivate(receiptPath, status); err != nil {
					t.Fatal(err)
				}
			case "receipt-generation":
				status.Generation = "ambiguous"
				if err := writePrivate(receiptPath, status); err != nil {
					t.Fatal(err)
				}
			case "receipt-digest":
				status.Input.Message = "changed"
				if err := writePrivate(receiptPath, status); err != nil {
					t.Fatal(err)
				}
			case "receipt-filename":
				if err := os.Rename(receiptPath, filepath.Join(root, "submissions", strings.Repeat("e", 32)+".json")); err != nil {
					t.Fatal(err)
				}
			case "unknown-workspace":
				workspace = t.TempDir()
			case "artifact":
				if err := os.WriteFile(filepath.Join(root, "unowned.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := privateInventory(t, state)
			if result, err := InspectRetainedUtilities(context.Background(), state, []string{workspace}); err == nil || result != nil {
				t.Fatal("bad retained state accepted", kind, result, err)
			}
			if !reflect.DeepEqual(before, privateInventory(t, state)) {
				t.Fatal("failed scan repaired state")
			}
		})
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectRetainedUtilities(canceled, t.TempDir(), []string{t.TempDir()}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
