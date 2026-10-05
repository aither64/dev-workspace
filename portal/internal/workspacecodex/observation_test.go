package workspacecodex

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/coder/websocket"
)

func TestLatestTurnObservationValidatesSemanticBoundaries(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, test := range []struct {
		name, payload string
		valid         bool
		date          bool
	}{
		{"empty", `{"data":[]}`, true, false},
		{"empty incomplete", `{"data":[],"nextCursor":"more"}`, false, false},
		{"historical missing times", `{"data":[{"id":"turn","status":"completed"}]}`, true, false},
		{"terminal failure", `{"data":[{"id":"turn","status":"failed","startedAt":900,"completedAt":950}]}`, true, true},
		{"interrupted", `{"data":[{"id":"turn","status":"interrupted","startedAt":900,"completedAt":950}]}`, true, true},
		{"null data", `{"data":null}`, false, false},
		{"missing data", `{}`, false, false},
		{"duplicate", `{"data":[],"data":[]}`, false, false},
		{"null row", `{"data":[null]}`, false, false},
		{"no identity", `{"data":[{"id":"","status":"completed"}]}`, false, false},
		{"unknown status", `{"data":[{"id":"turn","status":"unknown"}]}`, false, false},
		{"future", `{"data":[{"id":"turn","status":"completed","startedAt":1001}]}`, false, false},
		{"fractional", `{"data":[{"id":"turn","status":"completed","startedAt":950.5}]}`, false, false},
		{"reverse times", `{"data":[{"id":"turn","status":"completed","startedAt":950,"completedAt":900}]}`, false, false},
		{"busy completion", `{"data":[{"id":"turn","status":"inProgress","completedAt":950}]}`, false, false},
		{"oversized page", `{"data":[{"id":"one","status":"completed"},{"id":"two","status":"completed"}]}`, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			turn, err := decodeLatestTurn([]byte(test.payload), now)
			if (err == nil) != test.valid {
				t.Fatalf("turn=%#v, error=%v", turn, err)
			}
			if test.date && (turn.StartedAt == nil || turn.CompletedAt == nil || *turn.CompletedAt != 950) {
				t.Fatalf("lost terminal boundaries: %#v", turn)
			}
		})
	}
}

func TestObserveArchivedSemanticActivityPreservesColdQueueBoundary(t *testing.T) {
	identity, _, path := archiveProofFixture(t)
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", identity.CodexHome)
	loadedReads, turnReads := 0, 0
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for {
			request, err := readObject(connection)
			if err != nil {
				return nil
			}
			var result any
			switch request["method"] {
			case "thread/read":
				result = map[string]any{"thread": map[string]any{"id": identity.ThreadID, "cwd": identity.Cwd, "projectId": identity.ProjectID, "source": "vscode", "path": path, "status": map[string]any{"type": "notLoaded"}, "updatedAt": time.Now().Unix()}}
			case "thread/turns/list":
				turnReads++
				result = map[string]any{"data": []any{map[string]any{"id": "terminal-turn", "status": "completed", "startedAt": 900, "completedAt": 950}}, "nextCursor": nil, "backwardsCursor": nil}
			case "thread/loaded/list":
				loadedReads++
				result = map[string]any{"data": []any{}, "nextCursor": nil}
			case "thread/queue/list":
				return fmt.Errorf("cold archived queue API must never be called")
			default:
				return fmt.Errorf("unexpected archived observation request %#v", request)
			}
			if err := writeObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
				return err
			}
		}
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{SubmissionLedgerPath: filepath.Join(t.TempDir(), "attempts.json")})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := client.ObserveThreadIdentity(ctx, identity.ThreadID, identity.Cwd, identity.ProjectID, ArchiveArchived)
	if err != nil || !result.ActivityKnown || !result.Idle || result.LastActivityAt == nil || *result.LastActivityAt != 950 {
		t.Fatalf("archived observation=%#v %v", result, err)
	}
	if loadedReads < 2 || turnReads < 4 {
		t.Fatalf("archived reads not bracketed: loaded=%d turns=%d", loadedReads, turnReads)
	}
}
