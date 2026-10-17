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

	"github.com/aither64/codex-web/codex"
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

// A workspace launch routes native caller metadata. Fully bound legacy launches
// remain supported for retained queued policies; tool arguments never pick identity.
type teamMCPBinding struct {
	stateRoot, workspace, slug, rootThreadID, address, memberThreadID string
	devSession, hostProfile, workDir                                  string
	store                                                             *teamruntime.Store
}

type teamMCPInvocation struct {
	command, workDir  string
	args, environment []string
	message           string
	receipt           *bytes.Buffer
}

type teamMCPDependencies struct {
	run func(context.Context, teamMCPInvocation) error
}

func (dependencies teamMCPDependencies) withDefaults() teamMCPDependencies {
	if dependencies.run == nil {
		dependencies.run = func(ctx context.Context, invocation teamMCPInvocation) error {
			command := exec.CommandContext(ctx, invocation.command, invocation.args...)
			command.Dir = invocation.workDir
			command.Env = invocation.environment
			command.Stdin = strings.NewReader(invocation.message)
			// The CLI writes a roster receipt. Neither it nor its errors belong
			// on stdout, which is reserved for MCP JSON-RPC frames.
			command.Stdout, command.Stderr = teamMCPReceiptWriter{invocation.receipt}, io.Discard
			if err := command.Run(); err != nil {
				return err
			}
			if invocation.receipt == nil || invocation.receipt.Len() == 0 {
				return errors.New("missing host delivery receipt")
			}
			return nil
		}
	}
	return dependencies
}

func teamMCPCommand(args []string, input io.Reader, output, diagnostics io.Writer, dependencies teamMCPDependencies) error {
	dependencies = dependencies.withDefaults()
	flags := flag.NewFlagSet("team-mcp", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	stateRoot := flags.String("user-state-root", "", "private user state root")
	hostProfile := flags.String("host-profile", "", "selected workspace user profile")
	workspace := flags.String("workspace", "", "canonical workspace root")
	slug := flags.String("session-slug", "", "bound session slug")
	rootThreadID := flags.String("root-thread-id", "", "bound lead thread ID")
	address := flags.String("member-address", "", "bound member address")
	memberThreadID := flags.String("member-thread-id", "", "bound member thread ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !canonicalAbsolutePath(*stateRoot) || !canonicalAbsolutePath(*workspace) ||
		!canonicalAbsolutePath(*hostProfile) {
		return errors.New("team-mcp requires a canonical state root, host profile, workspace, session, lead thread, member address, and member thread")
	}
	legacy := *slug != "" || *rootThreadID != "" || *memberThreadID != "" || *address != ""
	if legacy && (!session.ValidSlug(*slug) || !teamMCPThreadIDPattern.MatchString(*rootThreadID) ||
		!teamMCPThreadIDPattern.MatchString(*memberThreadID) || !teamMCPAddressPattern.MatchString(*address)) {
		return errors.New("team-mcp legacy binding requires every exact session, lead and member identity")
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
	// Keep the profile symlink in this path. Each report must dispatch through
	// the currently selected generation, including from a retained MCP process.
	devSession := filepath.Join(*hostProfile, "bin", "dev-session")
	if info, err := os.Stat(devSession); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("team-mcp cannot find the selected profile dev-session command")
	}
	binding := teamMCPBinding{stateRoot: *stateRoot, workspace: *workspace, slug: *slug,
		rootThreadID: *rootThreadID, address: *address, memberThreadID: *memberThreadID,
		devSession: devSession, hostProfile: *hostProfile, workDir: filepath.Join(*workspace, "work", *slug), store: store}
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
		if member.State != "ready" || member.Thread != binding.memberThreadID || member.RemovedAt != nil || member.RetireIntent != "" {
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
		if strings.HasPrefix(key, "DEV_SESSION_") || key == "DEV_WORKSPACES_STATE" || key == "DEV_WORKSPACES_PROFILE" || key == "DEV_WORKSPACE_NAME" {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment,
		"DEV_WORKSPACES_STATE="+binding.stateRoot,
		"DEV_WORKSPACES_PROFILE="+binding.hostProfile,
		"DEV_SESSION_SLUG="+binding.slug,
		"DEV_SESSION_WORKSPACE="+binding.workspace,
		"DEV_SESSION_WORK_DIR="+binding.workDir,
		"DEV_SESSION_MEMBER_ADDRESS="+binding.address,
		"DEV_SESSION_EXPECTED_ROOT_THREAD_ID="+binding.rootThreadID,
		"DEV_SESSION_EXPECTED_MEMBER_THREAD_ID="+binding.memberThreadID,
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
	if binding.slug == "" {
		threadID, err := teamMCPNativeThread(params.Meta)
		if err != nil {
			return teamMCPToolResult("A valid native caller thread is required.", true)
		}
		reporter, err := binding.store.ResolveReporter(threadID)
		if err != nil {
			fmt.Fprintln(diagnostics, "team-mcp: native caller is not a current unique ready member")
			return teamMCPToolResult("This caller is not an active team member in this workspace.", true)
		}
		binding.slug, binding.rootThreadID = reporter.Slug, reporter.RootThreadID
		binding.address, binding.memberThreadID = reporter.Member.Address, threadID
		binding.workDir = filepath.Join(binding.workspace, "work", binding.slug)
	} else if len(params.Meta) != 0 {
		// A legacy process may omit metadata, but cannot contradict it.
		threadID, err := teamMCPNativeThread(params.Meta)
		if err != nil || threadID != binding.memberThreadID {
			return teamMCPToolResult("The native caller does not match this legacy report binding.", true)
		}
	}
	if err := binding.ready(); err != nil {
		fmt.Fprintln(diagnostics, "team-mcp: bound member is not ready")
		return teamMCPToolResult("This team member is no longer active in this session.", true)
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	invocation := binding.invocation(*arguments.Message, *arguments.MessageID)
	invocation.receipt = new(bytes.Buffer)
	if err := run(bounded, invocation); err != nil {
		// Never print child stderr: the report may appear in a downstream error.
		fmt.Fprintf(diagnostics, "team-mcp: report delivery failed (%T)\n", err)
		return teamMCPToolResult("The report could not be confirmed. Retry with the same message_id.", true)
	}
	if invocation.receipt.Len() > 0 {
		var receipt codex.SendReceipt
		if teamMCPDecode(invocation.receipt.Bytes(), &receipt) != nil || receipt.ClientUserMessageID != *arguments.MessageID {
			return teamMCPToolResult("Report receipt could not be confirmed. Retry with the same message_id.", true)
		}
		return teamMCPToolResult("Report sent to lead. Receipt: "+strings.TrimSpace(invocation.receipt.String()), false)
	}
	return teamMCPToolResult("Report sent to lead.", false)
}

// The child receipt is bounded independently of JSON-RPC output. Child stderr
// is never copied into a response, since downstream errors can include text.
type teamMCPReceiptWriter struct{ buffer *bytes.Buffer }

func (w teamMCPReceiptWriter) Write(p []byte) (int, error) {
	if w.buffer == nil || w.buffer.Len()+len(p) > teamMCPMaxLineBytes/2 {
		return 0, errors.New("report receipt exceeds bound")
	}
	return w.buffer.Write(p)
}

func teamMCPNativeThread(raw json.RawMessage) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", errors.New("native metadata must be an object")
	}
	seen := map[string]bool{}
	var threadID string
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return "", errors.New("invalid or duplicate native metadata")
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return "", err
		}
		if key == "threadId" && json.Unmarshal(value, &threadID) != nil {
			return "", errors.New("native thread ID must be a string")
		}
	}
	if _, err := decoder.Token(); err != nil {
		return "", err
	}
	if decoder.Decode(new(any)) != io.EOF || !teamMCPThreadIDPattern.MatchString(threadID) {
		return "", errors.New("missing or invalid native caller thread")
	}
	return threadID, nil
}
