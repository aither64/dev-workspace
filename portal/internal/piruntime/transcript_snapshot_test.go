package piruntime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func transcriptContractFixture(t *testing.T, m *Manager, start StartRequest, data string) {
	t.Helper()
	m.config.transcriptSnapshotContract = func(app string) (bool, error) {
		if app != start.AppStorePath {
			t.Fatalf("contract selected from %s, frozen worker package is %s", app, start.AppStorePath)
		}
		return decodeTranscriptSnapshotContract([]byte(data))
	}
}

// Frozen field set from consumed a43 worker.go, intentionally independent of
// workerSnapshot. This is the strict old-reader oracle, not a second wire type.
type frozenA43WorkerSnapshot struct {
	InstructionsDigest string            `json:"instructionsDigest,omitempty"`
	HistoryStart       int               `json:"historyStart"`
	HistoryTotal       int               `json:"historyTotal"`
	HistoryDigest      string            `json:"historyDigest"`
	NativeID           string            `json:"nativeId"`
	HistoryFile        string            `json:"historyFile"`
	Persisted          bool              `json:"persisted"`
	LeafID             string            `json:"leafId"`
	Entries            []json.RawMessage `json:"entries"`
	LiveMessage        json.RawMessage   `json:"liveMessage"`
	Busy               bool              `json:"busy"`
	Held               bool              `json:"held"`
	Model              string            `json:"model"`
	Provider           string            `json:"provider"`
	Thinking           string            `json:"thinking"`
}

func TestTranscriptSnapshotStrictContractAndEnvelope(t *testing.T) {
	for _, data := range []string{`{"other":1}`, `{"other":{},"piTranscriptSnapshot":1}`} {
		got, err := decodeTranscriptSnapshotContract([]byte(data))
		if err != nil || got != strings.Contains(data, "piTranscriptSnapshot") {
			t.Fatal(data, got, err)
		}
	}
	for _, data := range []string{`null`, `[]`, ``, `{"piTranscriptSnapshot":null}`, `{"piTranscriptSnapshot":true}`,
		`{"piTranscriptSnapshot":"1"}`, `{"piTranscriptSnapshot":1.0}`, `{"piTranscriptSnapshot":2}`,
		`{"piTranscriptSnapshot":1,"piTranscriptSnapshot":1}`, `{} {}`, strings.Repeat(" ", (1<<20)+1)} {
		if _, err := decodeTranscriptSnapshotContract([]byte(data)); err == nil {
			t.Fatal("invalid contract admitted", data[:min(len(data), 100)])
		}
	}
	legacy, _ := json.Marshal(workerSnapshot{NativeID: "native", Entries: []json.RawMessage{}})
	valid := `{"snapshot":` + string(legacy) + `,"acceptedInputs":[]}`
	var result transcriptSnapshot
	if strictDecode([]byte(valid), &result, recordLimit) != nil || result.Snapshot.NativeID != "native" {
		t.Fatal("strict envelope rejected")
	}
	for _, data := range []string{`null`, `{}`, `{"snapshot":` + string(legacy) + `}`, `{"snapshot":null,"acceptedInputs":[]}`,
		strings.Replace(valid, `"acceptedInputs":[]`, `"acceptedInputs":null`, 1),
		strings.TrimSuffix(valid, "}") + `,"forged":1}`, valid + `{}`,
		strings.Replace(valid, `"acceptedInputs":[]`, `"acceptedInputs":[],"acceptedInputs":[]`, 1),
		strings.Replace(valid, `"nativeId":"native"`, `"nativeId":"native","acceptedInputs":[]`, 1),
		strings.Replace(valid, `"acceptedInputs":[]`, `"acceptedInputs":[{"entryId":"entry","operationId":"bad","digest":"bad"}]`, 1)} {
		if strictDecode([]byte(data), &result, recordLimit) == nil {
			t.Fatal("invalid enriched response admitted", data)
		}
	}
	var native workerSnapshot
	if strictDecode([]byte(strings.Replace(string(legacy), `"nativeId":"native"`, `"nativeId":"native","acceptedInputs":[]`, 1)), &native, recordLimit) == nil {
		t.Fatal("native worker injected receipt identity")
	}
	var frozen frozenA43WorkerSnapshot
	if strictDecode(legacy, &frozen, recordLimit) != nil || strictDecode([]byte(valid), &frozen, recordLimit) == nil {
		t.Fatal("frozen legacy decoder oracle no longer distinguishes enriched wire")
	}
	if _, err := readTranscriptSnapshotContract("/nix/store/00000000000000000000000000000000-absent-contract"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing immutable contract was treated as legacy", err)
	}
	if _, err := readTranscriptSnapshotContract(filepath.Join(t.TempDir(), "package")); err == nil {
		t.Fatal("mutable package admitted")
	}
}

func TestTranscriptSnapshotReaderMatrixAndNoFallback(t *testing.T) {
	for _, declaration := range []string{`{"other":1}`, `{"piTranscriptSnapshot":1}`} {
		t.Run(declaration, func(t *testing.T) {
			m, host, w, h, ops, ctx, _ := adapterHTTPFixture(t)
			start, err := m.ReadStart(ctx, h.Conversation.Session)
			if err != nil {
				t.Fatal(err)
			}
			transcriptContractFixture(t, m, start, declaration)
			w.snapshot.Entries = []json.RawMessage{nativeRow("owned-user", "user", "wire", "", "")}
			c, _ := host.s.member(h.Conversation)
			ref := SubmissionRef{Conversation: h.Conversation, OperationID: "11111111-1111-4111-8111-111111111111", Action: "send"}
			input := SendInput{Message: "wire"}
			ref.Digest = SubmissionDigest(ref, input, c.record.Settings)
			receipt := SubmissionStatus{Schema: 1, Ref: ref, Phase: "Accepted", Generation: h.Generation, Settings: c.record.Settings, Input: input,
				Witness: &NativeWitness{SessionID: h.Conversation.NativeID, EntryID: "owned-user", EffectiveDigest: MessageDigest("wire")}}
			if err := c.receipts.save(receipt); err != nil {
				t.Fatal(err)
			}
			// The legacy-reader operation and frozen decoder work with either
			// legacy or enriched-capable supervisor package declarations.
			for _, before := range []*int{nil, workflowPointer(1)} {
				var frozen frozenA43WorkerSnapshot
				if err := m.handleCall(ctx, h, "snapshot", conversationRequest{Before: before}, &frozen); err != nil || len(frozen.Entries) != 1 {
					t.Fatal("old reader/current supervisor wire", frozen, err)
				}
			}
			transcript, err := ops.Read.Read(ctx, BackendIdentity(h))
			if err != nil || len(transcript.Entries) != 1 {
				t.Fatal(transcript, err)
			}
			want := ""
			if strings.Contains(declaration, "piTranscriptSnapshot") {
				want = ref.OperationID
			}
			if transcript.Entries[0].ClientUserMessageID != want {
				t.Fatal("current reader selected wrong frozen contract", transcript.Entries)
			}
			// A broken enriched proof must never retry the valid legacy read.
			receipt.Generation = strings.Repeat("f", 64)
			if err := c.receipts.save(receipt); err != nil {
				t.Fatal(err)
			}
			transcriptContractFixture(t, m, start, `{"piTranscriptSnapshot":1}`)
			calls := w.snapshots
			if _, err := ops.Read.Read(ctx, BackendIdentity(h)); err == nil || w.snapshots != calls+1 {
				t.Fatal("enriched proof error downgraded", err, w.snapshots-calls)
			}
			for _, data := range []string{`{"piTranscriptSnapshot":null}`, `{"piTranscriptSnapshot":2}`, `{"piTranscriptSnapshot":1,"piTranscriptSnapshot":1}`} {
				transcriptContractFixture(t, m, start, data)
				calls := w.snapshots
				if _, err := ops.Read.Read(ctx, BackendIdentity(h)); err == nil || w.snapshots != calls {
					t.Fatal("invalid declaration reached a native read", data, err)
				}
			}
			m.config.transcriptSnapshotContract = func(app string) (bool, error) {
				if app != start.AppStorePath {
					t.Fatal("wrong frozen package", app)
				}
				return false, os.ErrNotExist
			}
			calls = w.snapshots
			if _, err := ops.Read.Read(ctx, BackendIdentity(h)); !errors.Is(err, os.ErrNotExist) || w.snapshots != calls {
				t.Fatal("missing contract became legacy", err)
			}
			transcriptContractFixture(t, m, start, `{"piTranscriptSnapshot":1}`)
			c.worker = nil
			if _, err := ops.Read.Read(ctx, BackendIdentity(h)); err == nil {
				t.Fatal("unavailable enriched operation downgraded")
			}
			if w.sends != 0 || w.configures != 0 {
				t.Fatal("read changed native input/settings")
			}
		})
	}
}
