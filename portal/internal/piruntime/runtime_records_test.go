package piruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeSchema2RejectsOldReaderAndMalformedHistoryWithoutEffects(t *testing.T) {
	m, h, start, first, ctx := successorFixture(t)
	h.finish(t, first.Runtime)
	next := nextStart(start, "22222222-2222-4222-8222-222222222222")
	if _, err := m.Ensure(ctx, next); err != nil {
		t.Fatal(err)
	}
	directory, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	path := filepath.Join(directory, "runtime.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// This is the unchanged typed schema-1 record/strict reader, used by the
	// predecessor. It must refuse schema 2 rather than discard history.
	var old runtimeRecord
	if err := readPrivate(path, &old); err == nil {
		t.Fatal("schema-1 reader accepted runtime history")
	}
	for _, fault := range []string{"unknown-schema", "unknown-field", "duplicate-key", "missing-current", "orphan-root", "scope", "generation", "predecessor", "config-hash", "oversize"} {
		t.Run(fault, func(t *testing.T) {
			var record runtimeRecords
			if err := strictDecode(original, &record, recordLimit); err != nil {
				t.Fatal(err)
			}
			var data []byte
			switch fault {
			case "unknown-schema":
				record.Schema = 3
			case "missing-current":
				record.CurrentRequestID = "33333333-3333-4333-8333-333333333333"
			case "orphan-root":
				current := record.Requests[next.RequestID]
				current.Start.ReplacesRequestID = ""
				record.Requests[next.RequestID] = current
			case "scope":
				record.Session.Incarnation = "wrong"
			case "generation":
				current := record.Requests[next.RequestID]
				current.Runtime.Generation = "wrong"
				record.Requests[next.RequestID] = current
			case "predecessor":
				current := record.Requests[next.RequestID]
				current.Start.ReplacesRequestID = next.RequestID
				record.Requests[next.RequestID] = current
			case "config-hash":
				current := record.Requests[next.RequestID]
				current.RuntimeConfigSHA256 = "wrong"
				record.Requests[next.RequestID] = current
			case "oversize":
				data = bytes.Repeat([]byte(" "), recordLimit+1)
			case "unknown-field":
				data = append([]byte(`{"Unrecognized":true,`), original[1:]...)
			case "duplicate-key":
				data = append([]byte(`{"Schema":2,`), original[1:]...)
			}
			if data == nil {
				data, err = json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := m.records(start.Session); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatal("bad runtime map accepted as current/absent", fault, err)
			}
			if _, err := m.ReadStart(ctx, start.Session); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatal("bad map repaired as fresh start", fault, err)
			}
			if h.starts != 2 {
				t.Fatal("reader called native start", h.starts)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, data) {
				t.Fatal("reader normalized damaged map", err)
			}
		})
	}
}
