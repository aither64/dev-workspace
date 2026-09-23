package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

const (
	teamMCPProtocolVersion = "2025-06-18"
	teamMCPMaxLineBytes    = 64 * 1024
	teamMCPMaxMessage      = 20_000
)

var teamMCPThreadIDPattern = regexp.MustCompile(`^[0-9A-Za-z-]{1,128}$`)
var teamMCPAddressPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}[0-9]+$`)
var teamMCPMessageIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

// teamMCPBinding is fixed by App Server's per-thread MCP launch. Tool input
// cannot choose another session or sender.
type teamMCPBinding struct {
	stateRoot, workspace, slug, rootThreadID, address, memberThreadID string
	devSession, workDir                                               string
	store                                                             *teamruntime.Store
}

type teamMCPInvocation struct {
	command, workDir  string
	args, environment []string
	message           string
}

type teamMCPDependencies struct {
	executable func() (string, error)
	run        func(context.Context, teamMCPInvocation) error
}

func (dependencies teamMCPDependencies) withDefaults() teamMCPDependencies {
	if dependencies.executable == nil {
		dependencies.executable = os.Executable
	}
	if dependencies.run == nil {
		dependencies.run = func(ctx context.Context, invocation teamMCPInvocation) error {
			command := exec.CommandContext(ctx, invocation.command, invocation.args...)
			command.Dir = invocation.workDir
			command.Env = invocation.environment
			command.Stdin = strings.NewReader(invocation.message)
			// The CLI writes a roster receipt. Neither it nor its errors belong
			// on stdout, which is reserved for MCP JSON-RPC frames.
			command.Stdout, command.Stderr = io.Discard, io.Discard
			return command.Run()
		}
	}
	return dependencies
}

func teamMCPCommand(args []string, input io.Reader, output, diagnostics io.Writer, dependencies teamMCPDependencies) error {
	dependencies = dependencies.withDefaults()
	flags := flag.NewFlagSet("team-mcp", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	stateRoot := flags.String("user-state-root", "", "private user state root")
	workspace := flags.String("workspace", "", "canonical workspace root")
	slug := flags.String("session-slug", "", "bound session slug")
	rootThreadID := flags.String("root-thread-id", "", "bound lead thread ID")
	address := flags.String("member-address", "", "bound member address")
	memberThreadID := flags.String("member-thread-id", "", "bound member thread ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !canonicalAbsolutePath(*stateRoot) || !canonicalAbsolutePath(*workspace) ||
		!session.ValidSlug(*slug) || !teamMCPThreadIDPattern.MatchString(*rootThreadID) ||
		!teamMCPThreadIDPattern.MatchString(*memberThreadID) || !teamMCPAddressPattern.MatchString(*address) {
		return errors.New("team-mcp requires a canonical state root, workspace, session, lead thread, member address, and member thread")
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(*workspace)
	if err != nil || resolvedWorkspace != *workspace {
		return errors.New("team-mcp workspace must be an existing canonical directory")
	}
	info, err := os.Stat(*workspace)
	if err != nil || !info.IsDir() {
		return errors.New("team-mcp workspace must be an existing directory")
	}
	store, err := teamruntime.NewStore(*stateRoot, *workspace)
	if err != nil {
		return err
	}
	executable, err := dependencies.executable()
	if err != nil || !canonicalAbsolutePath(executable) {
		return errors.New("team-mcp cannot resolve its package executable")
	}
	devSession := filepath.Join(filepath.Dir(executable), "dev-session")
	if info, err := os.Stat(devSession); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("team-mcp cannot find the package dev-session command")
	}
	binding := teamMCPBinding{stateRoot: *stateRoot, workspace: *workspace, slug: *slug,
		rootThreadID: *rootThreadID, address: *address, memberThreadID: *memberThreadID,
		devSession: devSession, workDir: filepath.Join(*workspace, "work", *slug), store: store}
	return binding.serve(context.Background(), input, output, diagnostics, dependencies.run)
}

func (binding teamMCPBinding) ready() error {
	roster, err := binding.store.Load(binding.slug, binding.rootThreadID)
	if err != nil {
		return fmt.Errorf("load bound team roster: %w", err)
	}
	for _, member := range roster.Members {
		if member.Address != binding.address {
			continue
		}
		if member.State != "ready" || member.Thread != binding.memberThreadID || member.RemovedAt != nil {
			return errors.New("bound member is no longer ready on its original thread")
		}
		return nil
	}
	return errors.New("bound member is absent from its session roster")
}

func (binding teamMCPBinding) invocation(message, messageID string) teamMCPInvocation {
	// The host wrapper resolves the registered workspace from this fixed cwd,
	// then supplies its current authority, sockets, and generation guard.
	args := []string{"team", "assign", binding.slug, "--as-is", "--from", binding.address,
		"--to", "lead", "--message-stdin", "--message-id", messageID}
	environment := make([]string, 0, len(os.Environ())+7)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "DEV_SESSION_") || key == "DEV_WORKSPACES_STATE" || key == "DEV_WORKSPACE_NAME" {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment,
		"DEV_WORKSPACES_STATE="+binding.stateRoot,
		"DEV_SESSION_SLUG="+binding.slug,
		"DEV_SESSION_WORKSPACE="+binding.workspace,
		"DEV_SESSION_WORK_DIR="+binding.workDir,
		"DEV_SESSION_MEMBER_ADDRESS="+binding.address,
		"DEV_SESSION_EXPECTED_ROOT_THREAD_ID="+binding.rootThreadID,
	)
	return teamMCPInvocation{command: binding.devSession, workDir: binding.workDir,
		args: args, environment: environment, message: message}
}

type teamMCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type teamMCPResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *teamMCPError   `json:"error,omitempty"`
}

type teamMCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func teamMCPDecode(data []byte, value any) error {
	if !utf8.Valid(data) {
		return errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func teamMCPToolResult(message string, failed bool) map[string]any {
	return map[string]any{"isError": failed, "content": []any{map[string]any{"type": "text", "text": message}}}
}

func teamMCPValidID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true // A notification has no response.
	}
	if id[0] == '"' {
		var value string
		return json.Unmarshal(id, &value) == nil
	}
	return id[0] == '-' || id[0] >= '0' && id[0] <= '9'
}

func (binding teamMCPBinding) serve(ctx context.Context, input io.Reader, output, diagnostics io.Writer,
	run func(context.Context, teamMCPInvocation) error) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), teamMCPMaxLineBytes)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var request teamMCPRequest
		if err := teamMCPDecode(line, &request); err != nil {
			fmt.Fprintln(diagnostics, "team-mcp: invalid JSON-RPC request")
			if err := encoder.Encode(teamMCPResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &teamMCPError{Code: -32700, Message: "Invalid JSON request"}}); err != nil {
				return err
			}
			continue
		}
		if request.JSONRPC != "2.0" || request.Method == "" || !teamMCPValidID(request.ID) {
			if err := encoder.Encode(teamMCPResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &teamMCPError{Code: -32600, Message: "Invalid JSON-RPC request"}}); err != nil {
				return err
			}
			continue
		}
		if len(request.ID) == 0 {
			// Notifications do not receive responses or invoke host actions.
			continue
		}
		response := teamMCPResponse{JSONRPC: "2.0", ID: request.ID}
		switch request.Method {
		case "initialize":
			response.Result = map[string]any{"protocolVersion": teamMCPProtocolVersion,
				"capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo":   map[string]any{"name": "dev-workspace-team", "version": version}}
		case "ping":
			response.Result = map[string]any{}
		case "tools/list":
			response.Result = map[string]any{"tools": []any{map[string]any{
				"name":        "report_to_lead",
				"description": "Send a report or question to this session's lead. Reuse the same message_id when retrying an uncertain call.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"message":    map[string]any{"type": "string", "minLength": 1, "description": "Report text, at most 20,000 UTF-8 bytes."},
					"message_id": map[string]any{"type": "string", "description": "Stable lowercase UUID or 32-character hex ID for this report."},
				}, "required": []string{"message", "message_id"}, "additionalProperties": false},
			}}}
		case "tools/call":
			response.Result = binding.call(ctx, request.Params, diagnostics, run)
		default:
			response.Error = &teamMCPError{Code: -32601, Message: "Method not found"}
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("team-mcp input exceeded its size limit or could not be read: %w", err)
	}
	return nil
}

func (binding teamMCPBinding) call(ctx context.Context, raw json.RawMessage, diagnostics io.Writer,
	run func(context.Context, teamMCPInvocation) error) any {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      json.RawMessage `json:"_meta"`
	}
	if err := teamMCPDecode(raw, &params); err != nil || params.Name != "report_to_lead" {
		return teamMCPToolResult("Unknown tool or invalid tool call.", true)
	}
	var arguments struct {
		Message   *string `json:"message"`
		MessageID *string `json:"message_id"`
	}
	if err := teamMCPDecode(params.Arguments, &arguments); err != nil || arguments.Message == nil ||
		arguments.MessageID == nil || len(*arguments.Message) > teamMCPMaxMessage ||
		strings.TrimSpace(*arguments.Message) == "" || strings.ContainsRune(*arguments.Message, 0) ||
		!teamMCPMessageIDPattern.MatchString(*arguments.MessageID) {
		return teamMCPToolResult("Provide a nonempty message of at most 20,000 bytes and a stable lowercase UUID or 32-character hex message_id.", true)
	}
	if err := binding.ready(); err != nil {
		fmt.Fprintln(diagnostics, "team-mcp: bound member is not ready")
		return teamMCPToolResult("This team member is no longer active in this session.", true)
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := run(bounded, binding.invocation(*arguments.Message, *arguments.MessageID)); err != nil {
		// Never print child stderr: the report may appear in a downstream error.
		fmt.Fprintf(diagnostics, "team-mcp: report delivery failed (%T)\n", err)
		return teamMCPToolResult("The report could not be confirmed. Retry with the same message_id.", true)
	}
	return teamMCPToolResult("Report sent to lead.", false)
}
