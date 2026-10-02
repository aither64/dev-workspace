package workspacecodex

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/creationprogress"
	"github.com/coder/websocket"
)

func TestCreationRecoveryRequiresCompleteDiscovery(t *testing.T) {
	const cwd = "/workspace/work/example"
	row := func(id, project string) map[string]any {
		value := map[string]any{"id": id, "cwd": cwd, "source": "vscode"}
		if project != "" {
			value["projectId"] = project
		}
		return value
	}
	root, other, member := row("root", ""), row("other", "unknown-project"), row("member", "member-project")
	for _, test := range []struct {
		name                string
		loaded              []any
		loadedAfter         []any
		index               [][]any
		full                [][]any
		metadata            map[string]map[string]any
		options             RecoveryOptions
		indexError          bool
		fullError           bool
		repeatedIndexCursor bool
		wantError           string
		wantScans           int
	}{
		{name: "loaded only still scans", loaded: []any{"root"}, metadata: map[string]map[string]any{"root": root}, wantScans: 1},
		{name: "vanished loaded thread is rechecked", loaded: []any{"gone"}, loadedAfter: []any{}, full: [][]any{{root}}, wantScans: 1},
		{name: "still-loaded unreadable thread refuses", loaded: []any{"gone"}, wantError: "metadata unavailable", wantScans: 0},
		{name: "wrong returned loaded identity refuses", loaded: []any{"root"}, metadata: map[string]map[string]any{"root": other}, wantError: "wrong thread", wantScans: 0},
		{name: "positive index still scans", index: [][]any{{root}}, full: [][]any{{root}}, metadata: map[string]map[string]any{"root": root}, wantScans: 1},
		{name: "missing index uses full scan", full: [][]any{{root}}, wantScans: 1},
		{name: "failed index uses full scan", indexError: true, full: [][]any{{root}}, wantScans: 1},
		{name: "disappeared indexed row uses full scan", index: [][]any{{row("gone", "")}}, full: [][]any{{root}}, wantScans: 1},
		{name: "hidden unindexed duplicate refuses", index: [][]any{{root}}, full: [][]any{{root, other}}, metadata: map[string]map[string]any{"root": root}, wantError: "ambiguous", wantScans: 1},
		{name: "confirmed indexed ambiguity refuses early", index: [][]any{{root, other}}, metadata: map[string]map[string]any{"root": root, "other": other}, wantError: "ambiguous", wantScans: 0},
		{name: "member-only pages do not imply ambiguity", index: [][]any{{member}, {root}}, full: [][]any{{member}, {root}}, metadata: map[string]map[string]any{"root": root, "member": member}, options: RecoveryOptions{ExcludedThreads: map[string]string{"member": "member-project"}}, wantScans: 2},
		{name: "legacy exact member exclusion", full: [][]any{{member, root}}, options: RecoveryOptions{ExcludedThreads: map[string]string{"member": ""}}, wantScans: 1},
		{name: "unrecorded project thread counts", full: [][]any{{other, root}}, options: RecoveryOptions{ExcludedThreads: map[string]string{"member": "member-project"}}, wantError: "ambiguous", wantScans: 1},
		{name: "wrong member project refuses", index: [][]any{{member}}, metadata: map[string]map[string]any{"member": row("member", "wrong-project")}, options: RecoveryOptions{ExcludedThreads: map[string]string{"member": "member-project"}}, wantError: "member thread", wantScans: 0},
		{name: "repeated index cursor falls back", repeatedIndexCursor: true, full: [][]any{{root}}, wantScans: 1},
		{name: "full scan failure cannot prove absence", fullError: true, wantError: "scan failed", wantScans: 1},
		{name: "wrong source full row refuses", full: [][]any{{map[string]any{"id": "root", "cwd": cwd, "source": "cli"}}}, wantError: "invalid creation", wantScans: 1},
		{name: "wrong recorded loaded cwd refuses", loaded: []any{"root"}, metadata: map[string]map[string]any{"root": {"id": "root", "cwd": "/wrong", "source": "vscode"}}, wantError: "wrong identity", wantScans: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var scans atomic.Int32
			socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
				if err := handshake(connection); err != nil {
					return err
				}
				indexPage, fullPage, loadedCalls := 0, 0, 0
				for {
					request, err := readObject(connection)
					if err != nil {
						return nil
					}
					params, _ := request["params"].(map[string]any)
					var result map[string]any
					var failure string
					switch request["method"] {
					case "thread/loaded/list":
						loaded := test.loaded
						loadedCalls++
						if loadedCalls > 1 && test.loadedAfter != nil {
							loaded = test.loadedAfter
						}
						if loaded == nil {
							loaded = []any{}
						}
						result = map[string]any{"data": loaded}
					case "thread/read":
						if metadata := test.metadata[params["threadId"].(string)]; metadata != nil {
							result = map[string]any{"thread": metadata}
						} else {
							failure = "metadata unavailable"
						}
					case "thread/list":
						if params["cwd"] != cwd || params["archived"] != false || params["sortDirection"] != "asc" || fmt.Sprint(params["sourceKinds"]) != "[vscode]" {
							return fmt.Errorf("discovery filters changed: %#v", params)
						}
						pages, page := test.full, fullPage
						if params["useStateDbOnly"] == true {
							pages, page = test.index, indexPage
							indexPage++
							if test.indexError {
								failure = "index failed"
							}
						} else {
							if _, exists := params["useStateDbOnly"]; exists {
								return fmt.Errorf("full scan serialized false: %#v", params)
							}
							fullPage++
							scans.Add(1)
							if test.fullError {
								failure = "scan failed"
							}
						}
						data := []any{}
						if page < len(pages) {
							data = pages[page]
						}
						result = map[string]any{"data": data}
						if page+1 < len(pages) {
							result["nextCursor"] = fmt.Sprint(page + 1)
						}
						if params["useStateDbOnly"] == true && test.repeatedIndexCursor {
							result["nextCursor"] = "same"
						}
					default:
						return fmt.Errorf("discovery submitted a mutation: %#v", request)
					}
					response := map[string]any{"id": request["id"], "result": result}
					if failure != "" {
						delete(response, "result")
						response["error"] = map[string]any{"code": -32000, "message": failure}
					}
					if err := writeObject(connection, response); err != nil {
						return err
					}
				}
			})
			client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var stages []string
			options := test.options
			options.Progress = func(event creationprogress.Event) {
				if event.Event == "begin" {
					stages = append(stages, event.Stage)
				}
			}
			candidates, err := client.creationCandidates(ctx, "root", cwd, options)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("expected %q, got %v", test.wantError, err)
				}
			} else if err != nil || len(candidates) != 1 || candidates["root"].ID != "root" {
				t.Fatalf("candidates = %#v, %v", candidates, err)
			}
			if scans.Load() != int32(test.wantScans) {
				t.Fatalf("full scans = %d; stages = %v", scans.Load(), stages)
			}
		})
	}
}

func TestCreationRecoveryRetainedRosterPreventsReplacement(t *testing.T) {
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for _, method := range []string{"thread/loaded/list", "thread/list", "thread/list"} {
			request, err := readObject(connection)
			if err != nil || request["method"] != method {
				return fmt.Errorf("expected %s: %#v, %v", method, request, err)
			}
			if err := writeObject(connection, map[string]any{"id": request["id"], "result": map[string]any{"data": []any{}}}); err != nil {
				return err
			}
		}
		return nil
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.RecoverCreatingThreadWithOptions(ctx, "root", "/workspace/work/example", nil, codex.ThreadPolicy{},
		func() (codex.ThreadSettings, error) {
			t.Error("resolved replacement settings for retained roster")
			return codex.ThreadSettings{}, nil
		},
		RecoveryOptions{RetainedRoster: true, BeforeCreate: func() { t.Error("authorized new root") }})
	if err == nil || !strings.Contains(err.Error(), "roster prevents") {
		t.Fatalf("replacement = %v", err)
	}
}

func TestCreationRecoveryChecksRecordedIdentityBeforeReplacement(t *testing.T) {
	const cwd = "/workspace/work/example"
	for _, test := range []struct {
		name      string
		metadata  map[string]any
		code      int
		message   string
		want      string
		candidate bool
		retained  bool
	}{
		{name: "recorded cwd changed", metadata: map[string]any{"id": "root", "cwd": "/wrong", "source": "vscode"}, want: "wrong identity"},
		{name: "recorded source changed", metadata: map[string]any{"id": "root", "cwd": cwd, "source": "cli"}, want: "wrong identity"},
		{name: "recorded metadata contradicts empty scan", metadata: map[string]any{"id": "root", "cwd": cwd, "source": "vscode"}, want: "absent from complete discovery"},
		{name: "unknown read outcome", code: -32000, message: "metadata unavailable", want: "metadata unavailable"},
		{name: "selected no rollout does not prove disappearance", code: -32600, message: "no rollout found for thread id root", want: "no rollout found"},
		{name: "other no rollout does not prove disappearance", code: -32600, message: "no rollout found for thread id other", want: "no rollout found"},
		{name: "selected not loaded does not prove disappearance", code: -32600, message: "thread not loaded: root", want: "thread not loaded"},
		{name: "exact not found permits replacement", code: -32001, message: "thread not found"},
		{name: "other candidate cannot hide wrong recorded cwd", candidate: true, metadata: map[string]any{"id": "root", "cwd": "/wrong", "source": "vscode"}, want: "wrong identity"},
		{name: "other candidate requires recorded disappearance", candidate: true, code: -32001, message: "thread not found"},
		{name: "other candidate cannot retarget retained roster", candidate: true, retained: true, want: "roster prevents"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidateMetadata := freshThreadMetadata("other", cwd, filepath.Join(t.TempDir(), "missing.jsonl"))
			socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
				if err := handshake(connection); err != nil {
					return err
				}
				methods := []string{"thread/loaded/list", "thread/list", "thread/list"}
				if !test.retained {
					if test.candidate {
						methods = append(methods, "thread/read")
					}
					methods = append(methods, "thread/read")
				}
				for _, method := range methods {
					request, err := readObject(connection)
					if err != nil || request["method"] != method {
						return fmt.Errorf("expected %s: %#v, %v", method, request, err)
					}
					response := map[string]any{"id": request["id"], "result": map[string]any{"data": []any{}}}
					params, _ := request["params"].(map[string]any)
					if method == "thread/list" && params["useStateDbOnly"] != true && test.candidate {
						response["result"] = map[string]any{"data": []any{map[string]any{"id": "other", "cwd": cwd, "source": "vscode"}}}
					}
					if method == "thread/read" {
						if params["threadId"] == "other" {
							response["result"] = map[string]any{"thread": candidateMetadata}
						} else if params["threadId"] != "root" {
							return fmt.Errorf("exact root check = %#v", request)
						} else if test.metadata == nil {
							delete(response, "result")
							response["error"] = map[string]any{"code": test.code, "message": test.message}
						} else {
							response["result"] = map[string]any{"thread": test.metadata}
						}
					}
					if err := writeObject(connection, response); err != nil {
						return err
					}
				}
				if test.want != "" || test.candidate {
					return nil
				}
				request, err := readObject(connection)
				if err != nil || request["method"] != "thread/start" {
					return fmt.Errorf("expected authorized replacement: %#v, %v", request, err)
				}
				return writeObject(connection, map[string]any{"id": request["id"], "result": map[string]any{
					"thread": map[string]any{"id": "replacement", "cwd": cwd},
				}})
			})
			client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resolved := false
			id, err := client.RecoverCreatingThreadWithOptions(ctx, "root", cwd, nil, codex.ThreadPolicy{},
				func() (codex.ThreadSettings, error) { resolved = true; return codex.ThreadSettings{}, nil }, RecoveryOptions{RetainedRoster: test.retained})
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) || resolved {
					t.Fatalf("refusal = %q, %v; resolved = %v", id, err, resolved)
				}
			} else if test.candidate {
				if err != nil || id != "other" || resolved {
					t.Fatalf("adoption = %q, %v; resolved = %v", id, err, resolved)
				}
			} else if err != nil || id != "replacement" || !resolved {
				t.Fatalf("replacement = %q, %v; resolved = %v", id, err, resolved)
			}
		})
	}
}
