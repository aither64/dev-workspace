package workspacecodex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/coder/websocket"
)

// Uses the existing transport/handshake owner; these responses exercise only
// the local partition and saved-header contracts, not a second native protocol.
func absenceClient(t *testing.T, home string, reply func(string, map[string]any) (any, error)) *Client {
	t.Helper()
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for {
			request, err := readObject(connection)
			if err != nil {
				return nil
			} // client closes after the proof
			method := request["method"].(string)
			params, _ := request["params"].(map[string]any)
			result, responseErr := reply(method, params)
			response := map[string]any{"id": request["id"], "result": result}
			if responseErr != nil {
				delete(response, "result")
				response["error"] = map[string]any{"code": -32000, "message": responseErr.Error()}
			}
			if err := writeObject(connection, response); err != nil {
				return err
			}
		}
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	client.CodexHome = home
	t.Cleanup(client.Close)
	return client
}

func emptyAbsenceReply(method string, _ map[string]any) (any, error) {
	if method != "thread/list" && method != "project/list" {
		return nil, fmt.Errorf("unexpected RPC %s", method)
	}
	return map[string]any{"data": []any{}, "nextCursor": nil}, nil
}

func requireSavedAbsence(t *testing.T, client *Client) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return client.RequireSavedConversationAbsence(ctx, "/workspace/work/example")
}

func TestSavedAbsenceUsesFailClosedProjectPartitions(t *testing.T) {
	for _, scenario := range []string{"empty", "assigned hit", "unassigned hit", "index unavailable", "missing data", "null data", "thread cursor", "project cursor", "duplicate project", "project drift", "project unavailable", "missing projects", "null projects", "project page bound"} {
		t.Run(scenario, func(t *testing.T) {
			projects, lists := 0, 0
			client := absenceClient(t, t.TempDir(), func(method string, params map[string]any) (any, error) {
				if method == "project/list" {
					projects++
					switch scenario {
					case "project page bound":
						return map[string]any{"data": []any{}, "nextCursor": fmt.Sprintf("page-%d", projects)}, nil
					case "project unavailable":
						return nil, fmt.Errorf("state DB unavailable")
					case "missing projects":
						return map[string]any{}, nil
					case "null projects":
						return map[string]any{"data": nil}, nil
					}
					if params["limit"] != float64(100) || params["sortKey"] != "position" || params["sortDirection"] != "asc" {
						t.Errorf("project params=%#v", params)
					}
					if scenario == "project cursor" {
						return map[string]any{"data": []any{}, "nextCursor": "repeat"}, nil
					}
					rows := []any{map[string]any{"id": "project"}}
					if scenario == "duplicate project" {
						rows = append(rows, rows[0])
					}
					if scenario == "project drift" && projects == 2 {
						rows = []any{}
					}
					return map[string]any{"data": rows, "nextCursor": nil}, nil
				}
				if method != "thread/list" {
					return nil, fmt.Errorf("unexpected RPC %s", method)
				}
				lists++
				project, explicit := params["projectId"]
				providers, ok := params["modelProviders"].([]any)
				kinds, _ := json.Marshal(params["sourceKinds"])
				wantKinds, _ := json.Marshal(ArchiveDiscoverySourceKinds())
				if !explicit || (project != nil && project != "project") || params["useStateDbOnly"] != true || params["cwd"] != "/workspace/work/example" || params["limit"] != float64(1) || params["sortDirection"] != "asc" || !ok || len(providers) != 0 || string(kinds) != string(wantKinds) {
					t.Errorf("not a fail-closed bounded partition: %#v", params)
				}
				switch scenario {
				case "index unavailable":
					return nil, fmt.Errorf("state DB unavailable")
				case "missing data":
					return map[string]any{}, nil
				case "null data":
					return map[string]any{"data": nil}, nil
				case "thread cursor":
					return map[string]any{"data": []any{}, "nextCursor": "more"}, nil
				}
				if scenario == "assigned hit" && project != nil || scenario == "unassigned hit" && project == nil {
					return map[string]any{"data": []any{map[string]any{"id": proofThreadID}}}, nil
				}
				return emptyAbsenceReply(method, params)
			})
			if err := requireSavedAbsence(t, client); (err == nil) != (scenario == "empty") {
				t.Fatalf("proof=%v", err)
			}
			if scenario == "project page bound" && (projects != 64 || lists != 0) {
				t.Fatalf("project bound not enforced: %d, %d", projects, lists)
			}
			if scenario == "empty" && (projects != 2 || lists != 4) {
				t.Fatalf("incomplete partition proof: %d projects, %d queries", projects, lists)
			}
		})
	}
}

func TestSavedAbsenceCompletesProjectPagination(t *testing.T) {
	pages := 0
	partitions := map[string]int{}
	client := absenceClient(t, t.TempDir(), func(method string, params map[string]any) (any, error) {
		if method == "project/list" {
			pages++
			if params["cursor"] == nil {
				return map[string]any{"data": []any{map[string]any{"id": "first"}}, "nextCursor": "second-page"}, nil
			}
			if params["cursor"] != "second-page" {
				t.Errorf("cursor=%#v", params)
			}
			return map[string]any{"data": []any{map[string]any{"id": "second"}}, "nextCursor": nil}, nil
		}
		partitions[fmt.Sprint(params["projectId"])]++
		return emptyAbsenceReply(method, params)
	})
	if err := requireSavedAbsence(t, client); err != nil {
		t.Fatal(err)
	}
	if pages != 4 || !reflect.DeepEqual(partitions, map[string]int{"<nil>": 2, "first": 2, "second": 2}) {
		t.Fatalf("pages=%d partitions=%v", pages, partitions)
	}
}

func savedAbsenceFile(t *testing.T, home, cwd string) string {
	t.Helper()
	path := filepath.Join(home, "sessions", "2026", "10", "rollout-2026-10-05T10-00-00-"+proofThreadID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": proofThreadID, "cwd": cwd}})
	if err := os.WriteFile(path, append(header, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSavedAbsenceAccountsForUnindexedHeadersAndDrift(t *testing.T) {
	for _, scenario := range []string{"unrelated", "same CWD", "compressed same CWD", "compressed unrelated", "plain sibling", "malformed", "missing CWD", "wrong ID", "symlink", "replacement", "new file", "header drift", "body append", "missing root appears"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			cwd := "/workspace/work/another"
			if strings.Contains(scenario, "same CWD") {
				cwd = "/workspace/work/example"
			}
			path := savedAbsenceFile(t, home, cwd)
			switch scenario {
			case "compressed same CWD", "compressed unrelated":
				if err := os.Rename(path, path+".zst"); err != nil {
					t.Fatal(err)
				}
				path += ".zst"
			case "plain sibling":
				if err := os.WriteFile(path+".zst", []byte("unread compressed sibling"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				os.WriteFile(path, []byte(`{"type":"session_meta","type":"session_meta"}`+"\n"), 0o600)
			case "missing CWD":
				os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"`+proofThreadID+`"}}`+"\n"), 0o600)
			case "wrong ID":
				os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"other","cwd":"/different"}}`+"\n"), 0o600)
			case "symlink":
				os.Remove(path)
				os.Symlink("/missing", path)
			}
			projects, reads := 0, 0
			client := absenceClient(t, home, func(method string, params map[string]any) (any, error) {
				if method == "thread/read" {
					reads++
					if params["threadId"] != proofThreadID || params["excludeTurns"] != nil {
						t.Errorf("not exact metadata-only fallback: %#v", params)
					}
					if scenario == "malformed" || scenario == "missing CWD" {
						return nil, fmt.Errorf("scope unavailable")
					}
					return map[string]any{"thread": map[string]any{"id": proofThreadID, "cwd": cwd, "path": strings.TrimSuffix(path, ".zst")}}, nil
				}
				if method == "project/list" {
					projects++
					if projects == 2 {
						switch scenario {
						case "replacement":
							data, _ := os.ReadFile(path)
							os.Rename(path, path+".old")
							os.WriteFile(path, data, 0o600)
						case "new file":
							os.WriteFile(filepath.Join(filepath.Dir(path), "unexpected"), []byte("import"), 0o600)
						case "header drift":
							os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"`+proofThreadID+`","cwd":"/workspace/work/example"}}`+"\n"), 0o600)
						case "body append":
							file, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
							file.WriteString(strings.Repeat("turn body\n", 1<<17))
							file.Close()
						case "missing root appears":
							os.Mkdir(filepath.Join(home, "archived_sessions"), 0o700)
						}
					}
				}
				return emptyAbsenceReply(method, params)
			})
			wantSuccess := scenario == "unrelated" || scenario == "compressed unrelated" || scenario == "plain sibling" || scenario == "body append"
			if err := requireSavedAbsence(t, client); (err == nil) != wantSuccess {
				t.Fatalf("proof=%v", err)
			}
			if (scenario == "unrelated" || scenario == "body append" || scenario == "plain sibling") && reads != 0 {
				t.Fatal("read turn/native metadata despite a complete first header")
			}
			if scenario == "compressed unrelated" && reads != 2 {
				t.Fatalf("compressed proof not rechecked: %d", reads)
			}
		})
	}
}

type countedHeaderReader struct {
	reader io.Reader
	bytes  int
}

func (reader *countedHeaderReader) Read(data []byte) (int, error) {
	n, err := reader.reader.Read(data)
	reader.bytes += n
	return n, err
}

func TestSavedHeaderReaderBoundsPrefixRatherThanTurnBody(t *testing.T) {
	line := `{"type":"session_meta","payload":{"id":"` + proofThreadID + `","cwd":"/unrelated"}}` + "\n"
	reader := &countedHeaderReader{reader: io.MultiReader(strings.NewReader(line), io.LimitReader(zeroBody{}, 32<<20))}
	header, prefix, err := readArchiveHeaderRecord(reader)
	if err != nil || header.ID != proofThreadID || string(prefix) != line || reader.bytes > archiveHeaderLimit+1 {
		t.Fatalf("bounded header=%#v bytes=%d err=%v", header, reader.bytes, err)
	}
	if reader.bytes >= 32<<20 {
		t.Fatal("read the turn body")
	}
}

type zeroBody struct{}

func (zeroBody) Read(data []byte) (int, error) { clear(data); return len(data), nil }
