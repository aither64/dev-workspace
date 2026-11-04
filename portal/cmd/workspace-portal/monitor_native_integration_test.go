//go:build live_switch_integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
)

// These are the pinned 0.165 executor grammar and nested API, not a replacement
// model catalog. CodeModeOnly advertises the outer executor; native validates
// the nested call after the cell checks its actual registry identity.
const monitorExecGrammar = "start: pragma_source | plain_source\npragma_source: PRAGMA_LINE NEWLINE SOURCE\nplain_source: SOURCE\n\nPRAGMA_LINE: /[ \\t]*\\/\\/ @exec:[^\\r\\n]*/\nNEWLINE: /\\r?\\n/\nSOURCE: /[\\s\\S]+/"
const monitorResultMarker = "monitor-fixture-result:"

var monitorMCPUnavailable = fmt.Errorf("native monitor MCP function unavailable")

type monitorNativeCall struct {
	ID, OutputType, Kind, Slot, OperationID string
}

type monitorNativeResult struct {
	Kind, Output, ID, ThreadID, EventID, Slot string
	ExitCode                                  *int `json:"exit_code"`
	SessionID                                 *int `json:"session_id"`
	Schema                                    int
}

func monitorAdvertisedTools(top []map[string]any, input []map[string]any) ([]map[string]any, error) {
	tools := append([]map[string]any(nil), top...)
	for _, item := range input {
		if item["type"] != "additional_tools" {
			continue
		}
		items, ok := item["tools"].([]any)
		if item["role"] != "developer" || !ok {
			return nil, fmt.Errorf("invalid native additional_tools declaration")
		}
		for _, value := range items {
			tool, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid native additional_tools entry")
			}
			tools = append(tools, tool)
		}
	}
	return tools, nil
}

func monitorNativeToolCall(tools []map[string]any, id, kind, operationID string, arguments any, remaining time.Duration) (map[string]any, monitorNativeCall, error) {
	expected := monitorNativeCall{ID: id, Kind: kind, OperationID: operationID, OutputType: "function_call_output"}
	if kind != "shell" && kind != "mcp" {
		return nil, expected, fmt.Errorf("unsupported fixture call kind")
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, expected, err
	}
	var args map[string]any
	if json.Unmarshal(encoded, &args) != nil {
		return nil, expected, fmt.Errorf("fixture arguments must be an object")
	}
	expected.Slot, _ = args["slot"].(string)
	var executor map[string]any
	var directMCP map[string]any
	directShell := false
	seen := map[string]bool{}
	nodes := 0
	var walk func([]map[string]any, string, int) error
	walk = func(values []map[string]any, namespace string, depth int) error {
		for _, tool := range values {
			nodes++
			if nodes > 64 || depth > 2 {
				return fmt.Errorf("native tool declarations exceed fixture bound")
			}
			name, _ := tool["name"].(string)
			if tool["type"] == "namespace" {
				children, ok := tool["tools"].([]any)
				if name == "" || !ok || namespace != "" {
					return fmt.Errorf("invalid native namespace")
				}
				var nested []map[string]any
				for _, child := range children {
					value, ok := child.(map[string]any)
					if !ok {
						return fmt.Errorf("invalid native namespace entry")
					}
					nested = append(nested, value)
				}
				if err := walk(nested, name, depth+1); err != nil {
					return err
				}
				continue
			}
			if tool["type"] != "function" && tool["type"] != "custom" {
				continue
			}
			key := namespace + "/" + name
			if name == "" || seen[key] {
				return fmt.Errorf("ambiguous native tool identity")
			}
			seen[key] = true
			if tool["type"] == "function" && (name == "shell_command" || name == "exec_command") {
				directShell = true
			}
			if name == "exec" {
				format, _ := tool["format"].(map[string]any)
				grammar, _ := format["definition"].(string)
				if namespace != "functions" || tool["type"] != "custom" || format["type"] != "grammar" || format["syntax"] != "lark" || strings.TrimSpace(grammar) != monitorExecGrammar {
					return fmt.Errorf("native exec does not match pinned freeform grammar")
				}
				executor = tool
			}
			if namespace == "mcp__team_reporting" && name == "monitor_operation" && tool["type"] == "function" {
				directMCP = tool
			}
		}
		return nil
	}
	if err := walk(tools, "", 0); err != nil {
		return nil, expected, err
	}
	item := map[string]any{"id": id + "-item", "call_id": id}
	if executor != nil {
		if directShell || directMCP != nil {
			return nil, expected, fmt.Errorf("ambiguous direct and code-mode capabilities")
		}
		if remaining <= 0 {
			return nil, expected, fmt.Errorf("native case budget expired")
		}
		method := "mcp__team_reporting__monitor_operation"
		if kind == "shell" {
			method = "exec_command"
		}
		nameJSON, _ := json.Marshal(method)
		operationJSON, _ := json.Marshal(operationID)
		pragma, _ := json.Marshal(map[string]any{"yield_time_ms": min(remaining.Milliseconds(), int64(60000)), "max_output_tokens": 3000})
		code := "// @exec: " + string(pragma) + "\nconst name=" + string(nameJSON) + "; const args=" + string(encoded) + ";\n" +
			"if (!ALL_TOOLS.some(t => t.name === name) || typeof tools[name] !== 'function') throw new Error('Required native nested tool unavailable');\n" +
			"const result = await tools[name](args);\n"
		if kind == "shell" {
			if _, ok := args["cmd"].(string); !ok {
				return nil, expected, fmt.Errorf("invalid pinned shell command")
			}
			if _, ok := args["workdir"].(string); !ok || len(args) != 2 {
				return nil, expected, fmt.Errorf("invalid pinned shell arguments")
			}
			code += "if (!result || !Number.isInteger(result.exit_code) || result.session_id != null || typeof result.output !== 'string') throw new Error('Native shell did not finish');\n" +
				"text('" + monitorResultMarker + "'+JSON.stringify({kind:'shell',exit_code:result.exit_code,session_id:result.session_id,output:result.output}));"
		} else {
			code += "if (!result || result.isError === true || !Array.isArray(result.content)) throw new Error('Native MCP call failed');\n" +
				"const acknowledgements=result.content.filter(c=>c.type==='text').map(c=>JSON.parse(c.text));\n" +
				"if (acknowledgements.length !== 1) throw new Error('Ambiguous native MCP acknowledgement');\n" +
				"const ack=acknowledgements[0]; if (ack.id !== " + string(operationJSON) + " || ack.schema !== " + strconv.Itoa(monitoroperation.Schema) + ") throw new Error('Wrong native operation acknowledgement');\n" +
				"const event=args.slot ? ack.events?.find(e=>e.report.slot===args.slot) : null;\n" +
				"if (args.slot && !event?.id) throw new Error('Missing native report acknowledgement');\n" +
				"text('" + monitorResultMarker + "'+JSON.stringify({kind:'mcp',id:ack.id,schema:ack.schema,threadId:ack.launch?.threadId,eventId:event?.id,slot:event?.report.slot}));"
		}
		item["type"], item["name"], item["namespace"], item["input"] = "custom_tool_call", "exec", "functions", code
		expected.OutputType = "custom_tool_call_output"
		return item, expected, nil
	}
	item["type"] = "function_call"
	if kind == "shell" {
		return nil, expected, fmt.Errorf("direct shell calls are unsupported by this pinned fixture")
	}
	if directMCP == nil {
		return nil, expected, monitorMCPUnavailable
	}
	parameters, _ := directMCP["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	required, _ := json.Marshal(parameters["required"])
	action, _ := args["action"].(string)
	actionProperty, _ := properties["action"].(map[string]any)
	allowedJSON, _ := json.Marshal(actionProperty["enum"])
	var allowed []string
	_ = json.Unmarshal(allowedJSON, &allowed)
	validAction := false
	for _, candidate := range allowed {
		validAction = validAction || candidate == action
	}
	if parameters["type"] != "object" || parameters["additionalProperties"] != false || string(required) != "[\"action\"]" || !validAction {
		return nil, expected, fmt.Errorf("invalid direct MCP schema/action")
	}
	for key, value := range args {
		property, _ := properties[key].(map[string]any)
		valid := false
		switch value.(type) {
		case string:
			valid = property["type"] == "string"
		case float64:
			valid = property["type"] == "integer"
		case map[string]any:
			valid = property["type"] == "object"
		case []any:
			valid = property["type"] == "array"
		}
		if !valid {
			return nil, expected, fmt.Errorf("direct MCP argument is outside advertised schema")
		}
	}
	item["name"], item["namespace"] = "monitor_operation", "mcp__team_reporting"
	item["arguments"] = string(encoded)
	return item, expected, nil
}

// Root discovery is one native query, separate from the actual start call and
// its Operation acknowledgement. The utility's code-mode registry is unchanged.
type monitorRootDiscovery struct {
	SearchID  string
	StartJSON string
	StartSent bool
}

func (root *monitorRootDiscovery) next(tools, input []map[string]any, callID, operationID string, arguments any, remaining time.Duration) (map[string]any, *monitorNativeCall, error) {
	if root.StartSent {
		return nil, nil, nil
	}
	startJSON, err := json.Marshal(arguments)
	if err != nil {
		return nil, nil, err
	}
	if root.SearchID != "" {
		if root.StartJSON != string(startJSON) {
			return nil, nil, fmt.Errorf("monitor start changed during native discovery")
		}
		found := false
		var discovered []map[string]any
		for _, item := range input {
			if item["type"] != "tool_search_output" {
				continue
			}
			if found || item["call_id"] != root.SearchID || item["status"] != "completed" || item["execution"] != "client" {
				return nil, nil, fmt.Errorf("native discovery output identity/status mismatch")
			}
			found = true
			values, ok := item["tools"].([]any)
			encoded, err := json.Marshal(item["tools"])
			if !ok || len(values) == 0 || len(values) > 32 || err != nil || len(encoded) > 1024*1024 {
				return nil, nil, fmt.Errorf("native discovery definitions missing or outside bound")
			}
			for _, value := range values {
				tool, ok := value.(map[string]any)
				if !ok || (tool["type"] != "function" && tool["type"] != "namespace") {
					return nil, nil, fmt.Errorf("invalid native loadable tool definition")
				}
				discovered = append(discovered, tool)
			}
		}
		if !found {
			return nil, nil, fmt.Errorf("native discovery output missing; query will not repeat")
		}
		tools = discovered
	}
	call, expected, err := monitorNativeToolCall(tools, callID, "mcp", operationID, arguments, remaining)
	if err == nil {
		if expected.OutputType != "function_call_output" {
			return nil, nil, fmt.Errorf("root start requires an actual direct MCP definition")
		}
		root.StartSent = true
		return call, &expected, nil
	}
	if root.SearchID != "" || err != monitorMCPUnavailable {
		return nil, nil, err
	}
	var search map[string]any
	for _, tool := range tools {
		if tool["type"] != "tool_search" {
			continue
		}
		if search != nil {
			return nil, nil, fmt.Errorf("ambiguous native search advertisement")
		}
		search = tool
	}
	if search == nil || search["execution"] != "client" {
		return nil, nil, fmt.Errorf("native client discovery unavailable")
	}
	parameters, _ := search["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	query, _ := properties["query"].(map[string]any)
	required, _ := json.Marshal(parameters["required"])
	if parameters["type"] != "object" || parameters["additionalProperties"] != false || query["type"] != "string" || string(required) != "[\"query\"]" {
		return nil, nil, fmt.Errorf("unsupported native search schema")
	}
	for key, value := range properties {
		if key == "query" {
			continue
		}
		property, _ := value.(map[string]any)
		if key != "limit" || property["type"] != "number" {
			return nil, nil, fmt.Errorf("unsupported native search property")
		}
	}
	root.SearchID = operationID + "-discover-monitor"
	root.StartJSON = string(startJSON)
	return map[string]any{"type": "tool_search_call", "id": root.SearchID + "-item", "call_id": root.SearchID, "execution": "client", "arguments": map[string]any{"query": "monitor_operation"}}, nil, nil
}

// Native McpToolOutput prefixes text or inserts one header content item. Accept
// only that pinned framing and the candidate's single Operation text payload.
func monitorDirectMCPBody(value any) (string, error) {
	validHeader := func(text string) bool {
		return len(text) <= 128 && regexp.MustCompile(`^Wall time: [0-9]+\.[0-9]{4} seconds\nOutput:$`).MatchString(text)
	}
	if text, ok := value.(string); ok {
		if len(text) > 1024*1024 {
			return "", fmt.Errorf("direct MCP output exceeds fixture bound")
		}
		header, body, found := strings.Cut(text, "\nOutput:\n")
		if !found || !validHeader(header+"\nOutput:") {
			return "", fmt.Errorf("invalid direct MCP timing prefix")
		}
		return body, nil
	}
	blocks, ok := value.([]any)
	if !ok || len(blocks) != 2 {
		return "", fmt.Errorf("direct MCP requires one header and one payload block")
	}
	header, headerOK := blocks[0].(map[string]any)
	payload, payloadOK := blocks[1].(map[string]any)
	headerText, _ := header["text"].(string)
	body, bodyOK := payload["text"].(string)
	if !headerOK || !payloadOK || header["type"] != "input_text" || payload["type"] != "input_text" || !bodyOK ||
		!validHeader(headerText) || len(headerText)+len(body) > 1024*1024 {
		return "", fmt.Errorf("invalid direct MCP header/payload blocks")
	}
	return body, nil
}

func monitorNativeCallOutput(input monitorFixtureInput, expected monitorNativeCall, exitStatus int) (string, error) {
	if input.OutputCallID != expected.ID || input.OutputType != expected.OutputType {
		return "", fmt.Errorf("native tool output identity/type mismatch")
	}
	text := input.Output
	if expected.OutputType != "custom_tool_call_output" {
		if expected.Kind == "mcp" {
			// Direct MCP results contain the actual operation JSON, including its
			// instruction snapshot. Decode only identity fields, never print it.
			body, framingErr := monitorDirectMCPBody(input.RawOutput)
			var ack struct {
				ID      string
				Schema  int
				IsError bool
				Events  []struct {
					ID     string
					Report struct{ Slot string }
				}
			}
			if framingErr != nil || json.Unmarshal([]byte(body), &ack) != nil || ack.IsError || ack.ID != expected.OperationID || ack.Schema != monitoroperation.Schema {
				return "", fmt.Errorf("direct MCP acknowledgement mismatch")
			}
			if expected.Slot != "" {
				found := false
				for _, event := range ack.Events {
					if event.Report.Slot == expected.Slot && event.ID != "" {
						found = true
					}
				}
				if !found {
					return "", fmt.Errorf("direct MCP report acknowledgement missing")
				}
			}
			return "", nil
		}
		return "", fmt.Errorf("direct shell output is unsupported by this pinned fixture")
	}
	if !strings.Contains(text, "Script completed") || strings.Contains(text, "Script running") || strings.Contains(text, "Script failed") || strings.Contains(text, "Script terminated") || strings.Count(text, monitorResultMarker) != 1 {
		return "", fmt.Errorf("native code cell did not complete with one result")
	}
	_, projected, _ := strings.Cut(text, monitorResultMarker)
	line, _, _ := strings.Cut(projected, "\n")
	var result monitorNativeResult
	if json.Unmarshal([]byte(line), &result) != nil || result.Kind != expected.Kind {
		return "", fmt.Errorf("invalid native nested result")
	}
	if expected.Kind == "shell" {
		if result.ExitCode == nil || *result.ExitCode != exitStatus || result.SessionID != nil {
			return "", fmt.Errorf("native shell result is not the expected terminal exit")
		}
		return result.Output, nil
	}
	if result.ID != expected.OperationID || result.Schema != monitoroperation.Schema || (expected.Slot != "" && (result.Slot != expected.Slot || result.EventID == "")) {
		return "", fmt.Errorf("native MCP operation/report acknowledgement mismatch")
	}
	return "", nil
}

// Retain schema structure only. Never copy descriptions, defaults, grammar,
// instructions or other payload values from the advertised tool definitions.
func monitorToolSchemaSurface(tools []map[string]any) string {
	short := func(value any) string { text, _ := value.(string); return text[:min(len(text), 80)] }
	kind := func(value any) string {
		switch value.(type) {
		case map[string]any:
			return "object"
		case []any:
			return "array"
		case string:
			return "string"
		case nil:
			return "null"
		default:
			return "other"
		}
	}
	typeForm := func(value any) []string {
		values, ok := value.([]any)
		if !ok {
			values = []any{value}
		}
		var out []string
		for _, value := range values[:min(len(values), 4)] {
			switch name := short(value); name {
			case "string", "null", "object", "array", "integer", "number", "boolean":
				out = append(out, name)
			default:
				out = append(out, "unknown")
			}
		}
		return out
	}
	schemaForm := func(schema map[string]any) map[string]any {
		out := map[string]any{"type": typeForm(schema["type"])}
		for _, key := range []string{"anyOf", "oneOf"} {
			if children, ok := schema[key].([]any); ok {
				forms := make([][]string, 0, min(len(children), 4))
				for _, child := range children[:min(len(children), 4)] {
					entry, _ := child.(map[string]any)
					forms = append(forms, typeForm(entry["type"]))
				}
				out[key] = forms
			}
		}
		return out
	}
	var nodes []map[string]any
	truncated := false
	var visit func(map[string]any, string, int)
	visit = func(value map[string]any, namespace string, depth int) {
		if depth > 2 || len(nodes) == 32 {
			truncated = true
			return
		}
		wrappers := map[string]string{}
		for _, key := range []string{"function", "tools", "functions", "parameters", "input_schema", "format"} {
			if value[key] != nil {
				wrappers[key] = kind(value[key])
			}
		}
		entry := map[string]any{"type": short(value["type"]), "name": short(value["name"]), "namespace": namespace, "wrappers": wrappers}
		if value["namespace"] != nil {
			entry["declaredNamespace"] = short(value["namespace"])
		}
		if format, ok := value["format"].(map[string]any); ok {
			entry["formatType"] = short(format["type"])
		}
		for _, key := range []string{"parameters", "input_schema"} {
			if schema, ok := value[key].(map[string]any); ok {
				form := schemaForm(schema)
				names := []string{"command", "cmd", "workdir"}
				required, _ := schema["required"].([]any)
				list := make([]string, 0, min(len(required), 8))
				for _, field := range required[:min(len(required), 8)] {
					name := short(field)
					list = append(list, name)
					names = append(names, name)
				}
				form["required"] = list
				form["requiredTruncated"] = len(required) > 8
				properties, _ := schema["properties"].(map[string]any)
				fields := map[string]any{}
				for _, name := range names {
					if property, ok := properties[name].(map[string]any); ok {
						fields[name] = schemaForm(property)
					}
				}
				form["properties"] = fields
				entry[key] = form
			}
		}
		nodes = append(nodes, entry)
		if value["type"] == "namespace" {
			namespace = short(value["name"])
		}
		if child, ok := value["function"].(map[string]any); ok {
			visit(child, namespace, depth+1)
		}
		for _, key := range []string{"tools", "functions"} {
			if children, ok := value[key].([]any); ok {
				for _, child := range children {
					if entry, ok := child.(map[string]any); ok {
						visit(entry, namespace, depth+1)
					}
				}
			}
		}
	}
	for _, tool := range tools {
		visit(tool, "", 0)
	}
	data, _ := json.Marshal(map[string]any{"rootCount": len(tools), "nodes": nodes, "truncated": truncated})
	return string(data)
}

func TestMonitorToolSchemaSurface(t *testing.T) {
	const secret = "must-not-be-retained"
	tool := map[string]any{"type": "function", "name": "shell_command", "description": secret, "format": map[string]any{"type": "grammar", "definition": secret}, "function": map[string]any{"name": "wrapped", "description": secret},
		"parameters": map[string]any{"type": "object", "required": []any{"command", "workdir"}, "default": secret, "instructions": secret,
			"properties": map[string]any{"command": map[string]any{"type": "string", "default": secret, "description": secret},
				"workdir": map[string]any{"type": []any{"string", "null"}, "examples": []any{secret}}}}}
	surface := monitorToolSchemaSurface([]map[string]any{{"type": "namespace", "name": "functions", "tools": []any{tool}}})
	for _, wanted := range []string{"shell_command", "functions", "command", "workdir", "null", "wrapped", "grammar"} {
		if !strings.Contains(surface, wanted) {
			t.Fatalf("missing schema metadata %s: %s", wanted, surface)
		}
	}
	if strings.Contains(surface, secret) || strings.Contains(surface, "description") || strings.Contains(surface, "default") || strings.Contains(surface, "instructions") {
		t.Fatal("schema surface retained payload values")
	}
	var many []map[string]any
	for range 100 {
		many = append(many, tool)
	}
	var bounded struct {
		Nodes     []map[string]any `json:"nodes"`
		Truncated bool             `json:"truncated"`
	}
	if json.Unmarshal([]byte(monitorToolSchemaSurface(many)), &bounded) != nil || len(bounded.Nodes) > 32 || !bounded.Truncated {
		t.Fatal("schema surface exceeded node bound")
	}
}

func monitorRawRequestMetadata(raw json.RawMessage, leadModel, utilityModel string) map[string]any {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return map[string]any{"invalid": true}
	}
	kind := func(value json.RawMessage) string {
		value = bytes.TrimSpace(value)
		if len(value) == 0 {
			return "omitted"
		}
		switch value[0] {
		case '{':
			return "object"
		case '[':
			return "array"
		case '"':
			return "string"
		case 'n':
			return "null"
		case 't', 'f':
			return "boolean"
		default:
			return "number"
		}
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	top := map[string]string{}
	for _, key := range keys[:min(len(keys), 32)] {
		top[key[:min(len(key), 80)]] = kind(fields[key])
	}
	tools, toolsPresent := fields["tools"]
	var toolsCount *int
	if kind(tools) == "array" {
		var items []json.RawMessage
		if json.Unmarshal(tools, &items) == nil {
			count := len(items)
			toolsCount = &count
		}
	}
	wrappers := map[string]any{}
	for _, key := range []string{"request", "body", "response", "payload", "parameters", "model_parameters", "tool_namespaces", "functions", "tool_choice"} {
		value, present := fields[key]
		wrappers[key] = map[string]any{"present": present, "kind": kind(value)}
	}
	var model string
	_ = json.Unmarshal(fields["model"], &model)
	// Only retain identities already known from the fixture and installed policy.
	identity := "other"
	if model != "" && model == leadModel {
		identity = leadModel
	}
	if model != "" && model == utilityModel {
		identity = utilityModel
	}
	return map[string]any{"fieldCount": len(fields), "fields": top, "fieldsTruncated": len(keys) > 32,
		"tools": map[string]any{"present": toolsPresent, "kind": kind(tools), "count": toolsCount}, "wrappers": wrappers,
		"model": map[string]any{"kind": kind(fields["model"]), "identity": identity, "matchesLead": model != "" && model == leadModel,
			"matchesUtility": model != "" && model == utilityModel}}
}

func TestMonitorRawRequestMetadata(t *testing.T) {
	const secret = "must-not-be-retained"
	for _, test := range []struct {
		name    string
		tools   any
		present bool
		kind    string
		count   any
	}{
		{"omitted", nil, false, "omitted", nil}, {"null", nil, true, "null", nil},
		{"empty", []any{}, true, "array", 0}, {"array", []any{map[string]any{"description": secret}}, true, "array", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := map[string]any{"model": "fixture-luna", "instructions": secret, "input": []any{map[string]any{"content": secret}},
				"env": map[string]any{"value": secret}, "default": secret, "description": secret, "request": map[string]any{"body": secret}}
			if test.present {
				request["tools"] = test.tools
			}
			raw, _ := json.Marshal(request)
			meta := monitorRawRequestMetadata(raw, "fixture-lead", "fixture-luna")
			encoded, _ := json.Marshal(meta)
			if strings.Contains(string(encoded), secret) {
				t.Fatal("raw request metadata retained payload values")
			}
			var got struct {
				Tools struct {
					Present bool   `json:"present"`
					Kind    string `json:"kind"`
					Count   *int   `json:"count"`
				} `json:"tools"`
				Model struct {
					Identity       string `json:"identity"`
					MatchesUtility bool   `json:"matchesUtility"`
				} `json:"model"`
			}
			if json.Unmarshal(encoded, &got) != nil || got.Tools.Present != test.present || got.Tools.Kind != test.kind || got.Model.Identity != "fixture-luna" || !got.Model.MatchesUtility {
				t.Fatalf("raw request shape: %s", encoded)
			}
			if test.count == nil && got.Tools.Count != nil || test.count != nil && (got.Tools.Count == nil || *got.Tools.Count != test.count.(int)) {
				t.Fatalf("raw tools count: %s", encoded)
			}
			if !reflect.DeepEqual(meta["wrappers"].(map[string]any)["request"], map[string]any{"present": true, "kind": "object"}) ||
				!reflect.DeepEqual(meta["wrappers"].(map[string]any)["body"], map[string]any{"present": false, "kind": "omitted"}) {
				t.Fatalf("raw wrapper presence/kind: %s", encoded)
			}
		})
	}
	request := map[string]any{"model": secret}
	for i := range 50 {
		request[fmt.Sprintf("field-%02d-", i)+strings.Repeat("x", 100)] = secret
	}
	raw, _ := json.Marshal(request)
	meta := monitorRawRequestMetadata(raw, "fixture-lead", "fixture-luna")
	encoded, _ := json.Marshal(meta)
	if strings.Contains(string(encoded), secret) || len(meta["fields"].(map[string]string)) > 32 || meta["fieldsTruncated"] != true {
		t.Fatal("raw request metadata exceeded its value/filter bound")
	}
	for key := range meta["fields"].(map[string]string) {
		if len(key) > 80 {
			t.Fatal("raw request metadata exceeded its field-name bound")
		}
	}
}

func TestMonitorNativeToolAdapter(t *testing.T) {
	executor := func() map[string]any {
		return map[string]any{"type": "namespace", "name": "functions", "tools": []any{map[string]any{
			"type": "custom", "name": "exec", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "\n" + monitorExecGrammar + "\n"}}}}
	}
	mcp := func() map[string]any {
		return map[string]any{"type": "namespace", "name": "mcp__team_reporting", "tools": []any{map[string]any{
			"type": "function", "name": "monitor_operation", "parameters": monitorMCPTool()["inputSchema"]}}}
	}
	shell := map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{
		"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}, "workdir": map[string]any{"type": "string"}}, "required": []any{"cmd"}}}
	const operationID = "fixture-operation"
	arguments := map[string]any{"cmd": "printf '%s' 'quote\"; `text(99)`; ${value}\\n'", "workdir": "/fixture"}
	for _, test := range []struct {
		name       string
		tools      []map[string]any
		input      []map[string]any
		kind       string
		args       any
		outputType string
	}{
		{"Lite shell", nil, []map[string]any{{"type": "additional_tools", "role": "developer", "tools": []any{executor()}}}, "shell", arguments, "custom_tool_call_output"},
		{"Lite MCP", nil, []map[string]any{{"type": "additional_tools", "role": "developer", "tools": []any{executor()}}}, "mcp", map[string]any{"action": "report", "slot": "terminal"}, "custom_tool_call_output"},
		{"direct MCP", []map[string]any{mcp()}, nil, "mcp", map[string]any{"action": "status"}, "function_call_output"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tools, err := monitorAdvertisedTools(test.tools, test.input)
			if err != nil {
				t.Fatal(err)
			}
			item, expected, err := monitorNativeToolCall(tools, "exact-call", test.kind, operationID, test.args, time.Second)
			if err != nil || expected.ID != "exact-call" || expected.OutputType != test.outputType || item["call_id"] != "exact-call" {
				t.Fatalf("native call: %+v %+v %v", item, expected, err)
			}
			if expected.OutputType == "custom_tool_call_output" {
				code, _ := item["input"].(string)
				data, _ := json.Marshal(test.args)
				if item["type"] != "custom_tool_call" || item["namespace"] != "functions" || item["name"] != "exec" || item["arguments"] != nil ||
					!strings.Contains(code, "const args="+string(data)+";") || !strings.Contains(code, "ALL_TOOLS.some") || !strings.Contains(code, "typeof tools[name]") || !strings.Contains(code, "await tools[name](args)") {
					t.Fatal("native executor shape/argument serialization/registry guard changed")
				}
				// Parse generated code without executing any native or synthetic tool.
				check := exec.Command("node", "--check", "--input-type=module")
				check.Stdin = strings.NewReader(code)
				if output, err := check.CombinedOutput(); err != nil {
					t.Fatalf("generated JavaScript syntax: %s %v", output, err)
				}
				// An empty real registry must refuse before any nested invocation.
				refuse := exec.Command("node", "--input-type=module")
				refuse.Stdin = strings.NewReader("const ALL_TOOLS=[]; const tools={};\n" + code)
				if output, err := refuse.CombinedOutput(); err == nil || !strings.Contains(string(output), "Required native nested tool unavailable") {
					t.Fatalf("unavailable registry did not refuse: %s %v", output, err)
				}

			}
		})
	}
	if _, _, err := monitorNativeToolCall([]map[string]any{shell}, "call", "shell", operationID, arguments, time.Second); err == nil {
		t.Fatal("unsupported direct shell route accepted")
	}
	badGrammar := executor()
	badGrammar["tools"].([]any)[0].(map[string]any)["format"].(map[string]any)["definition"] = "SOURCE: /.+/"
	badNamespace := executor()
	badNamespace["name"] = "other"
	badType := executor()
	badType["tools"].([]any)[0].(map[string]any)["type"] = "function"
	for _, tools := range [][]map[string]any{nil, {badGrammar}, {badNamespace}, {badType}, {executor(), executor()}, {executor(), mcp()}, {executor(), shell}} {
		if _, _, err := monitorNativeToolCall(tools, "call", "shell", operationID, arguments, time.Second); err == nil {
			t.Fatal("missing/conflicting/unpinned capability accepted")
		}
	}
	for _, input := range [][]map[string]any{
		{{"type": "additional_tools", "role": "user", "tools": []any{executor()}}},
		{{"type": "additional_tools", "role": "developer", "tools": "invalid"}},
		{{"type": "additional_tools", "role": "developer", "tools": []any{"invalid"}}},
	} {
		if _, err := monitorAdvertisedTools(nil, input); err == nil {
			t.Fatal("malformed tool advertisement accepted")
		}
	}
}

func TestMonitorRootDiscovery(t *testing.T) {
	search := func() map[string]any {
		return map[string]any{"type": "tool_search", "execution": "client", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "limit": map[string]any{"type": "number"}}, "required": []any{"query"}, "additionalProperties": false}}
	}
	mcp := func() map[string]any {
		return map[string]any{"type": "namespace", "name": "mcp__team_reporting", "tools": []any{map[string]any{
			"type": "function", "name": "monitor_operation", "parameters": monitorMCPTool()["inputSchema"]}}}
	}
	args := map[string]any{"action": "start", "id": "operation", "brief": "unchanged fixture", "source": map[string]any{"kind": "local"}}
	output := func(id string) map[string]any {
		return map[string]any{"type": "tool_search_output", "call_id": id, "status": "completed", "execution": "client", "tools": []any{mcp()}}
	}
	var direct monitorRootDiscovery
	call, expected, err := direct.next([]map[string]any{mcp()}, nil, "start-call", "operation", args, time.Second)
	if err != nil || expected == nil || call["type"] != "function_call" || direct.SearchID != "" || !direct.StartSent {
		t.Fatalf("immediate direct path changed: %v", err)
	}
	var root monitorRootDiscovery
	call, expected, err = root.next([]map[string]any{search()}, nil, "original-call", "operation", args, time.Second)
	if err != nil || expected != nil || call["type"] != "tool_search_call" || call["execution"] != "client" || root.StartSent || root.SearchID == "original-call" ||
		!reflect.DeepEqual(call["arguments"], map[string]any{"query": "monitor_operation"}) {
		t.Fatalf("one native query: %+v %v", call, err)
	}
	queryID := root.SearchID
	items := []map[string]any{output(queryID)}
	if readMonitorFixtureInput(items).ToolOutput {
		t.Fatal("discovery was classified as operation acknowledgement")
	}
	call, expected, err = root.next([]map[string]any{search()}, items, "actual-start-call", "operation", args, time.Second)
	encoded, _ := json.Marshal(args)
	if err != nil || expected == nil || expected.ID != "actual-start-call" || expected.OutputType != "function_call_output" || !root.StartSent || root.SearchID != queryID ||
		call["type"] != "function_call" || call["namespace"] != "mcp__team_reporting" || call["name"] != "monitor_operation" || call["arguments"] != string(encoded) {
		t.Fatalf("discovery did not preserve actual unchanged start: %v", err)
	}
	if call, expected, err = root.next(nil, items, "duplicate", "operation", args, time.Second); err != nil || call != nil || expected != nil {
		t.Fatal("start/search was repeated after submission")
	}
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"wrong ID", func(v map[string]any) { v["call_id"] = "other" }},
		{"incomplete", func(v map[string]any) { v["status"] = "in_progress" }},
		{"missing execution", func(v map[string]any) { delete(v, "execution") }},
		{"server execution", func(v map[string]any) { v["execution"] = "server" }},
		{"empty", func(v map[string]any) { v["tools"] = []any{} }},
		{"malformed", func(v map[string]any) { v["tools"] = []any{"invalid"} }},
		{"wrong tool kind", func(v map[string]any) {
			v["tools"] = []any{map[string]any{"type": "custom", "name": "monitor_operation"}}
		}},
		{"alias", func(v map[string]any) {
			v["tools"] = []any{map[string]any{"type": "function", "name": "mcp__team_reporting__monitor_operation", "parameters": monitorMCPTool()["inputSchema"]}}
		}},
		{"conflicting", func(v map[string]any) { v["tools"] = []any{mcp(), mcp()} }},
		{"bound", func(v map[string]any) {
			many := make([]any, 33)
			for i := range many {
				many[i] = mcp()
			}
			v["tools"] = many
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := monitorRootDiscovery{SearchID: "query", StartJSON: string(encoded)}
			value := output("query")
			test.change(value)
			call, expected, err := state.next([]map[string]any{search()}, []map[string]any{value}, "call", "operation", args, time.Second)
			if err == nil || call != nil || expected != nil || state.StartSent || state.SearchID != "query" {
				t.Fatal("invalid search result admitted/replaced discovery")
			}
		})
	}
	for _, items := range [][]map[string]any{nil, {output("query"), output("query")}} {
		state := monitorRootDiscovery{SearchID: "query", StartJSON: string(encoded)}
		for range 2 {
			if call, expected, err := state.next([]map[string]any{search()}, items, "call", "operation", args, time.Second); err == nil || call != nil || expected != nil || state.SearchID != "query" {
				t.Fatal("missing/duplicate discovery retried")
			}
		}
	}
	changed := monitorRootDiscovery{SearchID: "query", StartJSON: string(encoded)}
	if call, expected, err := changed.next([]map[string]any{search()}, []map[string]any{output("query")}, "call", "operation", map[string]any{"action": "start", "id": "replacement"}, time.Second); err == nil || call != nil || expected != nil || changed.StartSent {
		t.Fatal("changed start crossed native discovery")
	}
	wrongExecution := search()
	wrongExecution["execution"] = "server"
	wrongSchema := search()
	wrongSchema["parameters"].(map[string]any)["required"] = []any{"query", "limit"}
	for _, tools := range [][]map[string]any{nil, {wrongExecution}, {wrongSchema}, {search(), search()}} {
		var state monitorRootDiscovery
		if call, expected, err := state.next(tools, nil, "call", "operation", args, time.Second); err == nil || call != nil || expected != nil || state.SearchID != "" {
			t.Fatal("invalid actual search advertisement accepted")
		}
	}
}

func TestMonitorNativeCallOutput(t *testing.T) {
	seven := 7
	zero := 0
	shell := monitorNativeCall{ID: "exact-call", OutputType: "custom_tool_call_output", Kind: "shell"}
	mcp := monitorNativeCall{ID: "exact-call", OutputType: "custom_tool_call_output", Kind: "mcp", OperationID: "operation", Slot: "terminal"}
	directMCP := mcp
	directMCP.OutputType = "function_call_output"
	directShell := shell
	directShell.OutputType = "function_call_output"

	completed := func(value any) string {
		encoded, _ := json.Marshal(value)
		return "Script completed\nOutput:\n" + monitorResultMarker + string(encoded)
	}
	const operation = `{"id":"operation","schema":1,"events":[{"id":"actual-event","report":{"slot":"terminal"}}]}`
	framedBlocks := func(body string) []any {
		return []any{map[string]any{"type": "input_text", "text": "Wall time: 0.0123 seconds\nOutput:"}, map[string]any{"type": "input_text", "text": body}}
	}
	goodShell := monitorNativeResult{Kind: "shell", ExitCode: &seven, Output: "native-monitor-exit=7"}
	goodMCP := monitorNativeResult{Kind: "mcp", ID: "operation", Schema: monitoroperation.Schema, Slot: "terminal", EventID: "real-event"}
	for _, test := range []struct {
		name     string
		expected monitorNativeCall
		id, typ  string
		value    any
		valid    bool
	}{
		{"exit7", shell, "exact-call", "custom_tool_call_output", completed(goodShell), true},
		{"direct shell unsupported", directShell, "exact-call", "function_call_output", `{"exit_code":7,"output":"native-monitor-exit=7"}`, false},
		{"direct MCP framed string", directMCP, "exact-call", "function_call_output", "Wall time: 0.0123 seconds\nOutput:\n" + operation, true},
		{"direct MCP framed blocks", directMCP, "exact-call", "function_call_output", framedBlocks(operation), true},
		{"direct MCP bare payload", directMCP, "exact-call", "function_call_output", operation, false},
		{"direct MCP malformed header", directMCP, "exact-call", "function_call_output", "Wall time: invalid seconds\nOutput:\n" + operation, false},
		{"direct MCP header only", directMCP, "exact-call", "function_call_output", "Wall time: 0.0123 seconds\nOutput:", false},
		{"direct MCP duplicate blocks", directMCP, "exact-call", "function_call_output", append(framedBlocks(operation), map[string]any{"type": "input_text", "text": operation}), false},
		{"direct MCP nontext", directMCP, "exact-call", "function_call_output", []any{map[string]any{"type": "input_text", "text": "Wall time: 0.0123 seconds\nOutput:"}, map[string]any{"type": "image"}}, false},
		{"direct MCP truncated", directMCP, "exact-call", "function_call_output", "Wall time: 0.0123 seconds\nOutput:\n" + operation[:len(operation)-1], false},
		{"direct MCP trailing payload", directMCP, "exact-call", "function_call_output", "Wall time: 0.0123 seconds\nOutput:\n" + operation + operation, false},
		{"direct MCP framed error", directMCP, "exact-call", "function_call_output", framedBlocks("Native tool error"), false},
		{"direct MCP bound", directMCP, "exact-call", "function_call_output", framedBlocks(strings.Repeat("x", 1024*1024)), false},
		{"direct MCP error", directMCP, "exact-call", "function_call_output", `{"isError":true,"content":[{"type":"text","text":"native refusal"}]}`, false},

		{"content blocks", shell, "exact-call", "custom_tool_call_output", []any{map[string]any{"type": "input_text", "text": completed(goodShell)}}, true},
		{"MCP acknowledgement", mcp, "exact-call", "custom_tool_call_output", completed(goodMCP), true},
		{"stale ID", shell, "old-call", "custom_tool_call_output", completed(goodShell), false},
		{"wrong type", shell, "exact-call", "function_call_output", completed(goodShell), false},
		{"marker only", shell, "exact-call", "custom_tool_call_output", monitorResultMarker + "{}", false},
		{"failed script", shell, "exact-call", "custom_tool_call_output", strings.Replace(completed(goodShell), "Script completed", "Script failed", 1), false},
		{"running cell", shell, "exact-call", "custom_tool_call_output", "Script running with cell ID owned\n" + completed(goodShell), false},
		{"terminated cell", shell, "exact-call", "custom_tool_call_output", "Script terminated\n" + completed(goodShell), false},
		{"missing exit", shell, "exact-call", "custom_tool_call_output", completed(monitorNativeResult{Kind: "shell"}), false},
		{"wrong exit", shell, "exact-call", "custom_tool_call_output", completed(monitorNativeResult{Kind: "shell", ExitCode: &zero}), false},
		{"duplicate result", shell, "exact-call", "custom_tool_call_output", completed(goodShell) + "\n" + completed(goodShell), false},
		{"live session", shell, "exact-call", "custom_tool_call_output", completed(monitorNativeResult{Kind: "shell", ExitCode: &seven, SessionID: &seven}), false},
		{"MCP wrong owner operation", mcp, "exact-call", "custom_tool_call_output", completed(monitorNativeResult{Kind: "mcp", ID: "other", Schema: monitoroperation.Schema, EventID: "event", Slot: "terminal"}), false},
		{"MCP no report ID", mcp, "exact-call", "custom_tool_call_output", completed(monitorNativeResult{Kind: "mcp", ID: "operation", Schema: monitoroperation.Schema, Slot: "terminal"}), false},
		{"MCP wrong slot", mcp, "exact-call", "custom_tool_call_output", completed(monitorNativeResult{Kind: "mcp", ID: "operation", Schema: monitoroperation.Schema, Slot: "escalation", EventID: "event"}), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := readMonitorFixtureInput([]map[string]any{{"type": test.typ, "call_id": test.id, "output": test.value}})
			_, err := monitorNativeCallOutput(input, test.expected, 7)
			if (err == nil) != test.valid {
				t.Fatalf("native result validity=%v err=%v", test.valid, err)
			}
		})
	}
}

type monitorFixtureInput struct {
	User, Output, OutputShape string
	OutputType, OutputCallID  string
	RawOutput                 any
	ToolOutput                bool
	CallIDs                   []string
}

func monitorFixtureText(value any) string {
	const limit = 8192
	if text, ok := value.(string); ok {
		return text[:min(len(text), limit)]
	}
	var text strings.Builder
	blocks, _ := value.([]any)
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block["type"] != "text" && block["type"] != "input_text" && block["type"] != "output_text" {
			continue
		}
		part, _ := block["text"].(string)
		text.WriteString(part[:min(len(part), limit-text.Len())])
		if text.Len() == limit {
			break
		}
	}
	return text.String()
}

func readMonitorFixtureInput(items []map[string]any) monitorFixtureInput {
	var input monitorFixtureInput
	for _, item := range items {
		if item["role"] == "user" {
			input = monitorFixtureInput{User: monitorFixtureText(item["content"])}
		}
		if item["type"] != "function_call_output" && item["type"] != "custom_tool_call_output" {
			continue
		}
		input.ToolOutput = true // Presence does not depend on output text or representation.
		input.RawOutput = item["output"]
		input.Output = monitorFixtureText(item["output"])
		input.OutputType, _ = item["type"].(string)
		input.OutputCallID, _ = item["call_id"].(string)
		switch item["output"].(type) {
		case string:
			input.OutputShape = "string"
		case []any:
			input.OutputShape = "array"
		case map[string]any:
			input.OutputShape = "object"
		case nil:
			input.OutputShape = "null"
		default:
			input.OutputShape = "other"
		}
		if id, ok := item["call_id"].(string); ok {
			input.CallIDs = append(input.CallIDs, id[:min(len(id), 128)])
			if len(input.CallIDs) > 8 {
				input.CallIDs = input.CallIDs[1:]
			}
		}
	}
	return input
}

func TestMonitorFixtureToolOutput(t *testing.T) {
	for _, test := range []struct {
		name        string
		value       any
		text, shape string
	}{
		{"string", "tool-result", "tool-result", "string"},
		{"empty", "", "", "string"},
		{"blocks", []any{map[string]any{"type": "input_text", "text": "native-"}, map[string]any{"type": "text", "text": "result"}}, "native-result", "array"},
		{"nontext", []any{map[string]any{"type": "image", "text": "ignored"}}, "", "array"},
		{"object", map[string]any{"text": "not a supported text block"}, "", "object"},
		{"null", nil, "", "null"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := readMonitorFixtureInput([]map[string]any{{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "monitor-fixture:start id"}}},
				{"type": "function_call_output", "call_id": "exact-call", "output": test.value}})
			if !input.ToolOutput || input.Output != test.text || input.OutputShape != test.shape || input.User != "monitor-fixture:start id" || !reflect.DeepEqual(input.CallIDs, []string{"exact-call"}) {
				t.Fatalf("tool output classification: %+v", input)
			}
		})
	}
	input := readMonitorFixtureInput([]map[string]any{{"role": "user", "content": "new user turn"}})
	if input.ToolOutput || input.User != "new user turn" {
		t.Fatal("fresh user input was classified as a tool result")
	}
	if got := monitorFixtureText([]any{map[string]any{"type": "text", "text": strings.Repeat("x", 9000)}}); len(got) != 8192 {
		t.Fatal("text normalization exceeded its fixed bound")
	}
}

// Only the model backend and the host-selection shell adapter are synthetic.
// The candidate's MCP and monitor CLI, native caller metadata, utility launch,
// ordinary portal worker and SQLite/native receipts remain production paths.
// LIVE_MONITOR_PACKAGE must be the built managed candidate, never a temp catalog.
func TestNativeMonitorOperation(t *testing.T) {
	native, candidate := os.Getenv("LIVE_SWITCH_CODEX"), os.Getenv("LIVE_MONITOR_PACKAGE")
	if !filepath.IsAbs(native) || !filepath.IsAbs(candidate) {
		t.Fatal("LIVE_SWITCH_CODEX and LIVE_MONITOR_PACKAGE must select exact built store outputs")
	}
	installed, err := agentteams.LoadInstalled(candidate)
	if err != nil || !installed.Managed || installed.Catalog == nil {
		t.Fatalf("native monitor fixture requires the real managed candidate catalog: %v", err)
	}
	version, err := exec.Command(native, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native=%s path=%s candidate=%s catalog=%s", bytes.TrimSpace(version), native, candidate, installed.Catalog.CatalogDigest)
	for _, mode := range []string{"passed", "failed", "escalation", "busy", "steer-race", "restart", "native-restart", "lost-reply", "cancel-uncertain", "cancel", "missing-report"} {
		t.Run(mode, func(t *testing.T) {
			monitorNativeRoundTrip(t, native, candidate, mode, installed.Catalog.Utilities.VerificationWatcher.Model)
		})
	}
}

func monitorNativeRoundTrip(t *testing.T, native, candidate, mode, utilityModel string) {
	f := newCommunicationCommandFixture(t)
	f.state = t.TempDir() // Portal private stores must be outside the workspace.
	home := filepath.Join(f.root, "codex-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const operationID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cwd := filepath.Join(f.root, "work", "example")
	evidence := filepath.Join(f.root, "command-evidence")
	exitEvidence := filepath.Join(f.root, "command-exit")
	commandRelease := filepath.Join(f.root, "release-command")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	script := "printf native-monitor-command > " + quote(evidence) + "; exit 0"
	exitStatus := 0
	if mode == "failed" {
		exitStatus = 7
		script = "printf native-monitor-command > " + quote(evidence) + "; exit 7"
	}
	if mode == "escalation" {
		script = "printf native-monitor-command > " + quote(evidence) + "; while [ ! -e " + quote(commandRelease) + " ]; do sleep 0.05; done; exit 0"
	}
	// This recorder observes exactly one invocation of Source.Argv. Escalation
	// instead observes a test-owned existing run, with an independently accessible
	// exit artifact written only after exec.Cmd.Wait proves that exact run ended.
	recorder := "sh -c " + quote(script) + "; monitor_result=$?; printf '%s\\n' \"$monitor_result\" > " + quote(exitEvidence) +
		"; printf 'native-monitor-exit=%s\\n' \"$monitor_result\"; exit \"$monitor_result\""
	command := "sh -c " + quote(recorder)
	start := monitoroperation.StartRequest{ID: operationID, Brief: "monitor-fixture:watch " + operationID,
		Source: monitoroperation.Source{Kind: "local", Argv: []string{"sh", "-c", script}, Cwd: cwd,
			InputIdentity: "isolated-native-monitor-fixture", Revision: strings.Repeat("1", 40), InvocationID: operationID}}
	report := monitoroperation.ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &exitStatus,
		Evidence: []string{evidence, exitEvidence}, ElapsedMillis: 1}
	if mode == "failed" {
		report.Outcome = "failed"
	}
	var mu sync.Mutex
	sequence, utilityPhase := 0, 0
	terminalReported := false
	var backendFailure string
	var externalPID int
	requestCounts := map[string]int{}
	pendingCalls := map[string]monitorNativeCall{}
	var rootDiscovery monitorRootDiscovery
	var lastRequest map[string]any
	utilityHeld := false
	requestEvidence := func() string {
		mu.Lock()
		defer mu.Unlock()
		data, _ := json.Marshal(map[string]any{"count": sequence, "roles": requestCounts, "last": lastRequest, "utilityPhase": utilityPhase, "utilityHeld": utilityHeld})
		return string(data)
	}
	utilityEntered, releaseUtility := make(chan struct{}), make(chan struct{})
	leadEntered, releaseLead := make(chan struct{}), make(chan struct{})
	var utilityOnce, utilityReleaseOnce, leadOnce, leadReleaseOnce sync.Once
	defer utilityReleaseOnce.Do(func() { close(releaseUtility) })
	defer leadReleaseOnce.Do(func() { close(releaseLead) })
	pidPattern := regexp.MustCompile(`native-monitor-pid=([0-9]+)`)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request struct {
			Input []map[string]any `json:"input"`
			Tools []map[string]any `json:"tools"`
		}
		var raw json.RawMessage
		if json.NewDecoder(r.Body).Decode(&raw) != nil || json.Unmarshal(raw, &request) != nil {
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		input := readMonitorFixtureInput(request.Input)
		lastUser, lastOutput := input.User, input.Output
		tools, advertisementError := monitorAdvertisedTools(request.Tools, request.Input)
		role := "other"
		switch {
		case strings.Contains(lastUser, "monitor-fixture:start "):
			role = "lead-start"
		case strings.Contains(lastUser, "monitor-fixture:hold"):
			role = "lead-hold"
		case strings.Contains(lastUser, "monitor-fixture:watch "):
			role = "utility"
		case strings.Contains(lastUser, "Monitoring operation outcome (runtime-owned)"):
			role = "lead-outcome"
		case strings.Contains(lastUser, "materialize-monitor-fixture"):
			role = "bootstrap"
		}
		mu.Lock()
		pending, awaitingOutput := pendingCalls[role]
		mu.Unlock()
		if advertisementError == nil && awaitingOutput {
			lastOutput, advertisementError = monitorNativeCallOutput(input, pending, exitStatus)
			if advertisementError == nil {
				mu.Lock()
				delete(pendingCalls, role)
				mu.Unlock()
			}
		}
		if advertisementError != nil {
			mu.Lock()
			backendFailure = advertisementError.Error()
			mu.Unlock()
		}
		mu.Lock()
		sequence++
		responseID := fmt.Sprintf("monitor-response-%d", sequence)
		requestCounts[role]++
		trace := map[string]any{"request": sequence, "role": role, "roleCount": requestCounts[role], "toolOutput": input.ToolOutput,
			"outputShape": input.OutputShape, "callIds": input.CallIDs, "utilityPhase": utilityPhase,
			"raw": monitorRawRequestMetadata(raw, "gpt-5.5", utilityModel)}
		lastRequest = trace
		traceJSON, _ := json.Marshal(trace)
		logRequest := sequence <= 12
		mu.Unlock()
		if logRequest {
			t.Logf("monitor-request %s", traceJSON)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(value any) { data, _ := json.Marshal(value); _, _ = fmt.Fprintf(w, "data: %s\n\n", data) }
		event(map[string]any{"type": "response.created", "response": map[string]any{"id": responseID}})
		w.(http.Flusher).Flush()
		item := map[string]any{"type": "message", "role": "assistant", "id": responseID + "-answer", "phase": "final_answer",
			"content": []any{map[string]any{"type": "output_text", "text": "monitor-native-fixture-complete"}}}
		nativeCall := func(kind string, arguments any) {
			deadline, _ := ctx.Deadline()
			call, expected, err := monitorNativeToolCall(tools, responseID+"-call", kind, operationID, arguments, time.Until(deadline))
			if err != nil {
				t.Logf("monitor-tool-schema %s", monitorToolSchemaSurface(tools))
				mu.Lock()
				backendFailure = err.Error()
				mu.Unlock()
				return
			}
			item = call
			mu.Lock()
			pendingCalls[role] = expected
			mu.Unlock()
		}
		if advertisementError != nil {
			// Complete the model response; the existing waiter reports the bounded
			// backend failure instead of permitting another tool mutation.
		} else if strings.Contains(lastUser, "monitor-fixture:start ") {
			deadline, _ := ctx.Deadline()
			mu.Lock()
			call, expected, err := rootDiscovery.next(tools, request.Input, responseID+"-call", operationID, struct {
				Action string `json:"action"`
				monitoroperation.StartRequest
			}{"start", start}, time.Until(deadline))
			if err != nil {
				backendFailure = err.Error()
			} else if call != nil {
				item = call
				if expected != nil {
					pendingCalls[role] = *expected
				}
			}
			mu.Unlock()
			if err != nil {
				t.Logf("monitor-tool-schema %s", monitorToolSchemaSurface(tools))
			}
		} else if strings.Contains(lastUser, "monitor-fixture:hold") && !input.ToolOutput {
			leadOnce.Do(func() { close(leadEntered) })
			select {
			case <-releaseLead:
			case <-r.Context().Done():
				return
			}
		} else if strings.Contains(lastUser, "monitor-fixture:watch ") {
			mu.Lock()
			phase := utilityPhase
			reported := terminalReported
			utilityPhase++
			mu.Unlock()
			if phase == 0 {
				mu.Lock()
				utilityHeld = true
				mu.Unlock()
				utilityOnce.Do(func() { close(utilityEntered) })
				select {
				case <-releaseUtility:
				case <-r.Context().Done():
					return
				}
				mu.Lock()
				utilityHeld = false
				mu.Unlock()
				mu.Lock()
				exactCommand := command
				mu.Unlock()
				nativeCall("shell", map[string]any{"cmd": exactCommand, "workdir": cwd})
			} else if !reported && mode == "escalation" && phase == 1 {
				match := pidPattern.FindStringSubmatch(lastOutput)
				if len(match) != 2 {
					mu.Lock()
					backendFailure = "escalation command returned no exact external process identity"
					mu.Unlock()
				} else {
					pid, _ := strconv.Atoi(match[1])
					mu.Lock()
					exactPID := externalPID
					mu.Unlock()
					if pid != exactPID {
						mu.Lock()
						backendFailure = "observation returned a different external process"
						mu.Unlock()
					}
					nativeCall("mcp", map[string]any{"action": "report", "slot": "escalation", "outcome": "incomplete",
						"elapsedMillis": 1, "evidence": []string{evidence}, "detail": "Fixture command awaits its release file",
						"stillRunning": fmt.Sprintf("operation %s exact recorder process %d", operationID, pid)})
				}
			} else if !reported && (phase == 1 || mode == "escalation") {
				if mode != "missing-report" {
					var recorded []byte
					var readErr error
					for {
						recorded, readErr = os.ReadFile(exitEvidence)
						if readErr == nil || !os.IsNotExist(readErr) {
							break
						}
						select {
						case <-r.Context().Done():
							return
						case <-time.After(20 * time.Millisecond):
						}
					}
					if readErr != nil || strings.TrimSpace(string(recorded)) != strconv.Itoa(exitStatus) {
						mu.Lock()
						backendFailure = "shell result did not prove the declared exit status"
						mu.Unlock()
					} else {
						mu.Lock()
						terminalReported = true
						mu.Unlock()
						nativeCall("mcp", struct {
							Action string `json:"action"`
							monitoroperation.ReportRequest
						}{"report", report})
					}
				}
			}
		}
		if item["type"] == "function_call" || item["type"] == "custom_tool_call" || item["type"] == "tool_search_call" {
			event(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
		}
		event(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		event(map[string]any{"type": "response.completed", "response": map[string]any{"id": responseID,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
	}))
	defer backend.Close()
	// The shim validates exact host selection, then execs the real candidate CLI.
	// It never fabricates operation state, a lead dispatcher or a native receipt.
	dispatcher := "#!/bin/sh\nset -eu\n" +
		"[ \"$#\" = 6 ] && [ \"$1\" = monitor ] && [ \"$3\" = example ] && [ \"$4\" = --as-is ] && [ \"$5\" = --request-stdin ] && [ \"$6\" = --json ]\n" +
		"[ \"$DEV_SESSION_WORKSPACE\" = " + quote(f.root) + " ] && [ \"$DEV_WORKSPACES_STATE\" = " + quote(f.state) + " ]\n" +
		"[ \"$PWD\" = " + quote(cwd) + " ] && [ -n \"$DEV_SESSION_EXPECTED_MONITOR_THREAD_ID\" ]\n" +
		"exec " + quote(filepath.Join(candidate, "bin/workspace-portal")) + " monitor --workspace " + quote(f.root) +
		" --user-state-root " + quote(f.state) + " --authority-dir " + quote(f.authority) + " --socket " + quote(f.socket) +
		" --session-slug example --expected-caller-thread-id \"$DEV_SESSION_EXPECTED_MONITOR_THREAD_ID\"\n"
	if err := os.WriteFile(filepath.Join(f.profile, "bin/dev-session"), []byte(dispatcher), 0700); err != nil {
		t.Fatal(err)
	}
	generation := filepath.Join(f.root, "host-generation")
	if err := os.Rename(f.profile, generation); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(generation, f.profile); err != nil {
		t.Fatal(err)
	}
	mcpArgs, _ := json.Marshal([]string{"team-mcp", "--user-state-root", f.state, "--workspace", f.root, "--host-profile", f.profile})
	config := "model_provider=\"fixture\"\nmodel=\"gpt-5.5\"\nmodel_reasoning_effort=\"low\"\napproval_policy=\"never\"\n" +
		"[model_providers.fixture]\nname=\"Monitor native fixture\"\nwire_api=\"responses\"\nsupports_websockets=false\nrequires_openai_auth=false\nbase_url=" + strconv.Quote(backend.URL+"/v1") + "\n" +
		"[mcp_servers.team_reporting]\ncommand=" + strconv.Quote(filepath.Join(candidate, "bin/workspace-portal")) + "\nargs=" + string(mcpArgs) +
		"\nrequired=true\nenabled_tools=[\"monitor_operation\",\"report_to_lead\"]\n[mcp_servers.team_reporting.tools.monitor_operation]\napproval_mode=\"approve\"\n" +
		"[mcp_servers.team_reporting.env]\nDEV_WORKSPACE_CODEX_HOME=" + strconv.Quote(home) + "\nDEV_WORKSPACE_NAME=\"\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(f.root, "native.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	nativeSocket := f.socket
	uncertain := mode == "lost-reply" || mode == "cancel-uncertain"
	if uncertain || mode == "steer-race" {
		nativeSocket = filepath.Join(f.root, "native.sock")
	}
	var daemon *exec.Cmd
	startNative := func() {
		t.Helper()
		daemon = exec.CommandContext(ctx, native, "app-server", "--strict-config", "--listen", "unix://"+nativeSocket)
		daemon.Dir, daemon.Stdout, daemon.Stderr = f.root, log, log
		daemon.Env = []string{"HOME=" + f.root, "CODEX_HOME=" + home, "PATH=" + os.Getenv("PATH"), "OPENAI_API_KEY=synthetic-only"}
		daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		actualNative, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", daemon.Process.Pid))
		selectedNative, resolveErr := filepath.EvalSymlinks(native)
		if err != nil || resolveErr != nil || actualNative != selectedNative {
			t.Fatalf("fixture daemon differs from exact selected native: actual=%s selected=%s read=%v resolve=%v", actualNative, selectedNative, err, resolveErr)
		}
		t.Logf("owned native daemon pid=%d executable=%s", daemon.Process.Pid, actualNative)
	}
	stopNative := func() {
		if daemon != nil && daemon.Process != nil {
			_ = syscall.Kill(-daemon.Process.Pid, syscall.SIGTERM)
			_ = daemon.Wait()
			daemon = nil
		}
	}
	defer func() {
		utilityReleaseOnce.Do(func() { close(releaseUtility) })
		leadReleaseOnce.Do(func() { close(releaseLead) })
		_ = os.WriteFile(commandRelease, []byte("release owned fixture command\n"), 0600)
		cancel()
		stopNative()
	}()
	startNative()
	latest := "waiting for native socket"
	wait := func(label string, check func() bool) {
		t.Helper()
		for !check() {
			mu.Lock()
			failure := backendFailure
			mu.Unlock()
			if failure != "" {
				t.Fatalf("%s: %s; requests=%s", label, failure, requestEvidence())
			}
			select {
			case <-ctx.Done():
				tail, _ := os.ReadFile(log.Name())
				t.Fatalf("%s: %v; latest=%s; requests=%s; native tail=%s", label, ctx.Err(), latest, requestEvidence(), tail[max(0, len(tail)-6000):])
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	wait("native socket", func() bool { info, err := os.Stat(nativeSocket); return err == nil && info.Mode()&os.ModeSocket != 0 })
	var barrier *retryNativeBarrier
	if uncertain {
		barrier = retryNativeReceiptBarrier(t, ctx, nativeSocket, f.socket)
	}
	var steerRace *monitorSteerRaceBarrier
	if mode == "steer-race" {
		steerRace = monitorNativeSteerRaceBarrier(t, ctx, nativeSocket, f.socket)
	}

	t.Setenv("DEV_WORKSPACE_NAME", "")
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", filepath.Join(f.root, "absent-providers.json"))
	bootstrap := codex.NewWithOptions(f.socket, codex.ClientOptions{PreserveThreadInstructionsOnResume: true})
	defer func() { bootstrap.Close() }()
	settings := codex.ThreadSettings{Model: "gpt-5.5", ReasoningEffort: "low", Policy: codex.ThreadPolicy{Sandbox: "danger-full-access", ApprovalPolicy: "never"}}
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	thread, err := bootstrap.StartThreadWithSettings(ctx, cwd, map[string]string{}, settings)
	if err != nil {
		t.Fatal(err)
	}
	f.session(t, "example", thread)
	epoch, err := session.SocketIdentity(f.socket)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := (session.RecoveryStore{Root: f.state, Workspace: f.root}).Load("example")
	if err != nil || !recovery.Active(thread, epoch) {
		t.Fatal("fixture lead lacks explicit execution permission at the exact native epoch", err)
	}
	manifest := fmt.Sprintf("schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.165.0\n", thread, f.socket)
	if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	var materialization struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := bootstrap.Request(ctx, "turn/start", map[string]any{"threadId": thread, "input": []any{map[string]any{"type": "text", "text": "materialize-monitor-fixture"}}}, &materialization); err != nil {
		t.Fatal(err)
	}
	if materialization.Turn.ID == "" {
		t.Fatal("bootstrap returned no exact materialization turn")
	}
	wait("materialized idle lead", func() bool { return bootstrap.RequireThreadIdle(ctx, thread, cwd) == nil })
	if mode == "escalation" {
		runCtx, stopRun := context.WithCancel(ctx)
		run := exec.CommandContext(runCtx, start.Source.Argv[0], start.Source.Argv[1:]...)
		run.Dir, run.Stdout, run.Stderr = cwd, log, log
		if err := run.Start(); err != nil {
			stopRun()
			t.Fatal(err)
		}
		mu.Lock()
		externalPID = run.Process.Pid
		command = fmt.Sprintf("kill -0 %d && printf 'native-monitor-pid=%d\\n'", externalPID, externalPID)
		start.Brief += fmt.Sprintf("\nObserve only the existing run at exact PID %d; read evidence %s and exit status %s. Do not start another invocation.", externalPID, evidence, exitEvidence)
		mu.Unlock()
		done := make(chan struct{})
		go func() {
			defer close(done)
			status := 0
			if err := run.Wait(); err != nil {
				status = -1
				if exited, ok := err.(*exec.ExitError); ok {
					status = exited.ExitCode()
				}
			}
			if err := os.WriteFile(exitEvidence, []byte(strconv.Itoa(status)+"\n"), 0600); err != nil {
				mu.Lock()
				backendFailure = "exact external-run exit evidence could not be retained"
				mu.Unlock()
			}
		}()
		defer func() {
			_ = os.WriteFile(commandRelease, []byte("release owned existing fixture run\n"), 0600)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				stopRun()
				<-done
			}
			stopRun()
		}()
	}
	client := newCodexClient(f.socket, f.root, f.state)
	defer func() { client.Close() }()
	teamStore, err := teamruntime.NewStore(f.state, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rosterBefore, err := teamStore.Load("example", thread)
	if err != nil {
		t.Fatal(err)
	}
	store, err := monitoroperation.NewStore(f.state, f.root)
	if err != nil {
		t.Fatal(err)
	}
	var application *portalweb.Server
	startPortal := func() {
		t.Helper()
		var err error
		application, err = portalweb.NewContext(ctx, portalweb.Config{Workspace: f.root, UserStateRoot: f.state,
			AuthorityDir: f.authority, CodexSocket: f.socket, CodexVersion: "0.165.0", Codex: client,
			BaseURL: "https://monitor-fixture.example.test", HostProfile: f.profile,
			DevSession: filepath.Join(f.profile, "bin/dev-session"), ObserveActivity: true, RecoverSessions: true,
			Logger: stdlog.New(log, "portal: ", 0)})
		if err != nil {
			t.Fatal(err)
		}
	}
	startPortal()
	defer func() {
		if application != nil {
			application.Close()
		}
	}()
	// An empty fresh history can pass generic idle checks before bootstrap is
	// recorded. Require this accepted turn to finish before the initial input.
	wait("bootstrap completed before initial lead input", func() bool {
		state, err := client.ReadTurnState(ctx, thread)
		return err == nil && state.ThreadID == thread && state.LatestTurnID == materialization.Turn.ID &&
			state.LatestTurnStatus == "completed" && state.Status != "active" && client.Client.RequireThreadIdle(ctx, thread, cwd) == nil
	})
	initial, err := client.SendWithOptions(ctx, thread, "monitor-fixture:start "+operationID, "native-monitor-initial", "fixture", codex.TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	originJSON, _ := json.Marshal(map[string]any{"rootThreadId": thread, "origin": initial})
	t.Logf("monitor-origin %s", originJSON)
	var operation monitoroperation.Operation
	var lastColdState codex.TurnState
	lastBinding := ""
	readLogged := false
	wait("persisted running utility and ended idle lead", func() bool {
		observed, readErr := store.Get(ctx, operationID)
		if readErr == nil {
			operation = observed
			binding, _ := json.Marshal(map[string]any{"operationId": operation.ID, "rootThreadId": operation.Owner.RootThreadID,
				"originTurnId": operation.Owner.OriginTurnID, "utilityThreadId": operation.Launch.ThreadID,
				"utilityProjectId": operation.Launch.ProjectID, "launchState": operation.Launch.State, "launchReceipt": operation.Launch.Receipt})
			if string(binding) != lastBinding {
				lastBinding = string(binding)
				t.Logf("monitor-binding %s", binding)
			}
		}
		if !readLogged {
			t.Logf("monitor-cold-read rootThreadId=%s expectedOriginTurnId=%s lastValid=%+v", thread, initial.TurnID, lastColdState)
			readLogged = true
		}
		state, stateErr := client.ReadTurnState(ctx, thread)
		if stateErr == nil {
			lastColdState = state
		}
		latest = fmt.Sprintf("binding=%s read=%v lastValidLead=%+v error=%v", lastBinding, readErr, lastColdState, stateErr)
		return readErr == nil && operation.Launch.Receipt != nil && stateErr == nil && state.LatestTurnID == initial.TurnID &&
			state.LatestTurnStatus == "completed" && state.Status != "active" && client.RequireThreadIdle(ctx, thread, cwd) == nil
	})
	select {
	case <-utilityEntered:
	case <-ctx.Done():
		t.Fatal("runtime-created utility never reached synthetic provider")
	}
	if operation.Owner.OriginTurnID != initial.TurnID || !reflect.DeepEqual(operation.Source, start.Source) ||
		operation.UtilityPolicy.PackageRoot != candidate || operation.Launch.ThreadID == thread {
		t.Fatal("operation did not retain exact owner/source/selected policy and distinct utility")
	}
	installed, err := agentteams.LoadInstalled(candidate)
	if err != nil {
		t.Fatal(err)
	}
	watcher := installed.Catalog.Utilities.VerificationWatcher
	utilitySettings, err := client.ReadThreadSettings(ctx, operation.Launch.ThreadID)
	if err != nil || operation.UtilityPolicy.CatalogDigest != installed.Catalog.CatalogDigest ||
		operation.UtilityPolicy.Model != watcher.Model || operation.UtilityPolicy.Effort != watcher.Effort ||
		utilitySettings.Model != watcher.Model || utilitySettings.ReasoningEffort != watcher.Effort {
		t.Fatal("runtime utility did not use the exact installed policy snapshot", err)
	}
	call := func(caller string, arguments any, wantError bool) monitoroperation.Operation {
		t.Helper()
		var result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := client.Request(ctx, "mcpServer/tool/call", map[string]any{"threadId": caller, "server": "team_reporting", "tool": "monitor_operation", "arguments": arguments,
			"_meta": map[string]string{"threadId": "forged-caller"}}, &result); err != nil {
			t.Fatal(err)
		}
		if result.IsError != wantError || len(result.Content) != 1 {
			t.Fatalf("monitor native MCP result: %+v", result)
		}
		var got monitoroperation.Operation
		if !wantError && json.Unmarshal([]byte(result.Content[0].Text), &got) != nil {
			t.Fatalf("invalid real monitor acknowledgement: %s", result.Content[0].Text)
		}
		return got
	}
	call(thread, map[string]any{"action": "report", "slot": "terminal", "outcome": "passed", "exitStatus": 0, "elapsedMillis": 0}, true)
	call(operation.Launch.ThreadID, map[string]any{"action": "cancel", "id": operationID}, true)
	var held codex.SendReceipt
	var raceLeadSettings codex.ThreadSettings
	var raceEvent monitoroperation.Event
	var raceOwner monitoroperation.Owner
	if mode == "steer-race" {
		raceLeadSettings, err = client.ReadThreadSettings(ctx, thread)
		if err != nil {
			t.Fatal(err)
		}
		raceOwner = operation.Owner
	}
	if mode == "busy" || mode == "steer-race" {
		held, err = client.SendWithOptions(ctx, thread, "monitor-fixture:hold", "native-monitor-held", "fixture", codex.TurnOptions{})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-leadEntered:
		case <-ctx.Done():
			t.Fatal("busy lead did not enter")
		}
	}
	if mode == "restart" || mode == "native-restart" || mode == "steer-race" {
		application.Close()
		application = nil
	}
	if mode == "cancel" {
		call(thread, map[string]any{"action": "cancel", "id": operationID}, false)
	}
	if uncertain {
		barrier.Store(true)
	}
	utilityReleaseOnce.Do(func() { close(releaseUtility) })
	if mode == "steer-race" {
		wait("race outcome persisted while delivery is paused", func() bool {
			operation, err = store.Get(ctx, operationID)
			return err == nil && len(operation.Events) == 1
		})
		raceEvent = operation.Events[0]
		currentEpoch, epochErr := session.SocketIdentity(f.socket)
		if epochErr != nil || currentEpoch != epoch || !reflect.DeepEqual(operation.Owner, raceOwner) ||
			!reflect.DeepEqual(operation.Source, start.Source) || raceEvent.Delivery.MessageID != raceEvent.ID {
			t.Fatal("race staging changed owned event/source/epoch")
		}
		if err := steerRace.arm(thread, raceEvent.Delivery.MessageID, held.TurnID); err != nil {
			t.Fatal(err)
		}
		passive := codex.NewWithOptions(nativeSocket, codex.ClientOptions{ObserverOnly: true})
		defer passive.Close()
		startPortal()
		wait("exact monitor steer held before native admission", func() bool {
			state := steerRace.snapshot()
			if state.Failure != "" {
				t.Fatal(state.Failure)
			}
			return state.Held
		})
		leadReleaseOnce.Do(func() { close(releaseLead) })
		// The original steer still owns its submission claim. Read only native
		// turn/status here; complete ledger idle proof follows settlement below.
		wait("held native turn completed before unchanged steer", func() bool {
			state, readErr := passive.ReadTurnState(ctx, thread)
			return readErr == nil && state.ThreadID == thread && state.LatestTurnID == held.TurnID && state.LatestTurnStatus == "completed" && state.Status == "idle"
		})
		steerRace.forwardHeld()
		wait("actual native no-active steer refusal", func() bool {
			state := steerRace.snapshot()
			if state.Failure != "" {
				t.Fatal(state.Failure)
			}
			return state.Refusals == 1
		})
	}
	if mode == "restart" || mode == "native-restart" {
		wait("outcome persisted without portal", func() bool { op, err := store.Get(ctx, operationID); return err == nil && len(op.Events) == 1 })
		if mode == "native-restart" {
			utilityID, projectID := operation.Launch.ThreadID, operation.Launch.ProjectID
			launchTurnID := operation.Launch.Receipt.TurnID
			utilityCwd := filepath.Join(operation.Workspace, "work", operation.Owner.Slug)
			binding, _ := json.Marshal(map[string]any{"utilityThreadId": utilityID, "utilityProjectId": projectID, "launchTurnId": launchTurnID, "nativeCwd": utilityCwd})
			t.Logf("monitor-native-restart-binding %s", binding)
			// The observer is paused. Prove the exact native launch directly and
			// preserve bounded evidence instead of relying on stored timestamps.
			var lastUtilityState codex.TurnState
			lastNonContextErrors := map[string]string{}
			rpcCode := regexp.MustCompile(`Codex RPC (-?[0-9]{1,10}):`)
			errorKind := func(err error) string {
				if err == nil {
					return "none"
				}
				if errors.Is(err, context.DeadlineExceeded) {
					return "deadline"
				}
				if errors.Is(err, context.Canceled) {
					return "canceled"
				}
				if codex.IsThreadNotFound(err, utilityID) {
					return "thread-not-found"
				}
				message := err.Error()
				switch {
				case message == "Codex thread does not match the trusted working directory":
					return "thread-identity-or-cwd"
				case strings.HasPrefix(message, "Codex thread "+utilityID+" is not idle ("):
					return "latest-turn-not-terminal"
				case strings.HasPrefix(message, "inspect pending Codex requests:"):
					return "prompt-read"
				case strings.HasPrefix(message, "inspect queued Codex messages:"):
					return "queue-read"
				case strings.HasPrefix(message, "Codex thread "+utilityID+" has "):
					switch {
					case strings.HasSuffix(message, " pending request(s)"):
						return "pending-prompts"
					case strings.HasSuffix(message, " queued message(s)"):
						return "queued-work"
					default:
						return "unresolved-submission-work"
					}
				}
				if code := rpcCode.FindStringSubmatch(message); len(code) == 2 {
					return "rpc:" + code[1]
				}
				return fmt.Sprintf("unclassified:%T", err) // Never print native error/prompt payloads.
			}
			wait("utility ended before native restart", func() bool {
				metadata, metadataErr := bootstrap.ReadThreadMetadata(ctx, utilityID, true)
				identity := metadataErr == nil && metadata.ID == utilityID && metadata.Cwd == utilityCwd && metadata.ProjectID != nil && *metadata.ProjectID == projectID
				state, readErr := bootstrap.ReadTurnState(ctx, utilityID)
				if readErr == nil {
					lastUtilityState = state
				}
				// SQLite import fences bootstrap's legacy JSON ledger. Keep the
				// same SDK idle guards on the configured runtime's shared store.
				idleErr := client.Client.RequireThreadIdle(ctx, utilityID, utilityCwd)
				mu.Lock()
				pending, awaitingOutput := pendingCalls["utility"]
				mu.Unlock()
				for stage, observedErr := range map[string]error{"metadata": metadataErr, "read": readErr, "idle": idleErr} {
					kind := errorKind(observedErr)
					if kind != "deadline" && kind != "canceled" {
						lastNonContextErrors[stage] = kind
					}
				}
				shortID := func(id string) string { return id[:min(len(id), 128)] }
				diagnostic, _ := json.Marshal(map[string]any{"utilityThreadId": utilityID, "utilityProjectId": projectID, "launchTurnId": launchTurnID,
					"identityMatched": identity, "metadataError": errorKind(metadataErr), "readError": errorKind(readErr), "idleError": errorKind(idleErr),
					"lastNonContextErrors": lastNonContextErrors,
					"lastValidUtility": map[string]string{"threadId": shortID(lastUtilityState.ThreadID), "status": lastUtilityState.Status,
						"turnId": shortID(lastUtilityState.LatestTurnID), "turnStatus": lastUtilityState.LatestTurnStatus},
					"pendingCallId": shortID(pending.ID), "pendingOutputType": pending.OutputType})
				latest = string(diagnostic)
				return identity && readErr == nil && state.ThreadID == utilityID && state.LatestTurnID == launchTurnID &&
					state.LatestTurnStatus == "completed" && state.Status != "active" && idleErr == nil && !awaitingOutput
			})
			retainedSettings, err := client.ReadThreadSettings(ctx, thread)
			if err != nil {
				t.Fatal(err)
			}
			bootstrap.Close()
			client.Close()
			stopNative()
			if err := os.Remove(nativeSocket); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			startNative()
			wait("new native socket epoch", func() bool { current, err := session.SocketIdentity(f.socket); return err == nil && current != epoch })
			bootstrap = codex.NewWithOptions(f.socket, codex.ClientOptions{PreserveThreadInstructionsOnResume: true})
			client = newCodexClient(f.socket, f.root, f.state)
			startPortal()
			wait("native restart holds pending continuation", func() bool {
				op, err := store.Get(ctx, operationID)
				return err == nil && len(op.Events) == 1 && op.Events[0].Delivery.Status == "held" && op.Events[0].Delivery.Receipt == nil
			})
			loaded, err := client.LoadedThreadIDs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range loaded {
				if id == operation.Launch.ThreadID {
					t.Fatal("native restart silently reloaded the utility")
				}
			}
			// Explicit fixture activation affects only the lead. The old utility
			// and command are never resumed, relaunched or assigned another ID.
			if _, err := bootstrap.ResumeThreadWithSettings(ctx, thread, cwd, map[string]string{}, retainedSettings); err != nil {
				t.Fatal(err)
			}
			if err := (session.RecoveryStore{Root: f.state, Workspace: f.root}).Set(ctx, "example", thread, f.socket, true, true); err != nil {
				t.Fatal(err)
			}
		} else {
			startPortal()
		}
	}
	if uncertain {
		wait("genuine accepted delivery with lost reply", func() bool {
			op, err := store.Get(ctx, operationID)
			return err == nil && len(op.Events) == 1 && op.Events[0].Delivery.Status == "submitting"
		})
		starts, accepted, _, nativeThread, nativeID, nativeTurn, failure := barrier.snapshot()
		if starts != 1 || accepted != 1 || nativeThread != thread || nativeTurn == "" || failure != "" {
			t.Fatalf("lost reply was not backed by one real native acceptance: starts=%d accepted=%d thread=%s turn=%s failure=%s", starts, accepted, nativeThread, nativeTurn, failure)
		}
		operation, _ = store.Get(ctx, operationID)
		if nativeID != operation.Events[0].ID {
			t.Fatal("uncertain delivery changed its saved native message ID")
		}
		if mode == "cancel-uncertain" {
			call(thread, map[string]any{"action": "cancel", "id": operationID}, false)
		}
		application.Close()
		application = nil
		client.Close()
		barrier.Store(false)
		client = newCodexClient(f.socket, f.root, f.state)
		startPortal()
	}
	wait("first event recorded/delivered", func() bool {
		operation, err = store.Get(ctx, operationID)
		if err != nil || len(operation.Events) == 0 {
			return false
		}
		delivery := operation.Events[0].Delivery
		latest = fmt.Sprintf("event status=%s detail=%s receipt=%+v", delivery.Status, delivery.Detail, delivery.Receipt)
		if mode == "cancel" {
			return delivery.Status == "canceled"
		}
		return delivery.Status == "accepted" && delivery.Receipt != nil && delivery.Receipt.TurnID != "" && delivery.Receipt.QueuedSubmissionID == ""
	})
	if mode == "escalation" {
		if operation.Events[0].Report.Slot != "escalation" || operation.Events[0].Report.Outcome != "incomplete" || operation.Events[0].Report.StillRunning == "" {
			t.Fatal("expected escalation before terminal")
		}
		mu.Lock()
		pid := externalPID
		mu.Unlock()
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatal("escalation did not retain the exact running external command", err)
		}
		wait("escalation continuation ended idle", func() bool { return client.RequireThreadIdle(ctx, thread, cwd) == nil })
		if err := os.WriteFile(commandRelease, []byte("release owned fixture command\n"), 0600); err != nil {
			t.Fatal(err)
		}
		wait("authoritative terminal after escalation", func() bool {
			operation, err = store.Get(ctx, operationID)
			return err == nil && len(operation.Events) == 2 && operation.Events[1].Delivery.Status == "accepted"
		})
	}
	leadReleaseOnce.Do(func() { close(releaseLead) })
	wait("utility native termination observed", func() bool {
		operation, err = store.Get(ctx, operationID)
		mu.Lock()
		_, awaitingOutput := pendingCalls["utility"]
		mu.Unlock()
		return err == nil && operation.Launch.TerminatedAt != nil && !awaitingOutput
	})
	wait("lead completed actual continuation", func() bool { return client.RequireThreadIdle(ctx, thread, cwd) == nil })
	mu.Lock()
	failure, pendingCount := backendFailure, len(pendingCalls)
	mu.Unlock()
	if failure != "" || pendingCount != 0 {
		t.Fatalf("native calls did not finish: failure=%s pending=%d", failure, pendingCount)
	}

	if mode != "missing-report" && !reflect.DeepEqual(operation.Events[len(operation.Events)-1].Report, report) {
		t.Fatal("native outcome differs from the actual command's authoritative result")
	}
	if _, err := os.Stat(evidence); err != nil {
		t.Fatalf("owned verification command was not executed: %v", err)
	}
	if mode == "steer-race" {
		state := steerRace.snapshot()
		event := operation.Events[0]
		afterSettings, settingsErr := client.ReadThreadSettings(ctx, thread)
		if state.Failure != "" || state.Steers != 1 || state.Refusals != 1 || state.Starts != 1 || state.Accepted != 1 || state.AcceptedTurn == "" ||
			state.Thread != thread || state.ClientID != event.ID || state.ExpectedTurn != held.TurnID || event.Delivery.Receipt == nil ||
			event.Delivery.Receipt.Steered || event.Delivery.Receipt.TurnID != state.AcceptedTurn || state.AcceptedTurn == held.TurnID {
			t.Fatalf("repair lacks one real refused steer and same-ID accepted idle start: %+v", state)
		}
		if !reflect.DeepEqual(operation.Owner, raceOwner) || !reflect.DeepEqual(operation.Source, start.Source) ||
			event.ID != raceEvent.ID || event.Delivery.MessageID != raceEvent.Delivery.MessageID || event.Delivery.Envelope != raceEvent.Delivery.Envelope ||
			event.Delivery.Context != raceEvent.Delivery.Context || !reflect.DeepEqual(event.Report, raceEvent.Report) || event.Delivery.Options == nil ||
			event.Delivery.Options.Model != raceLeadSettings.Model || event.Delivery.Options.ReasoningEffort != raceLeadSettings.ReasoningEffort ||
			settingsErr != nil || !reflect.DeepEqual(afterSettings, raceLeadSettings) {
			t.Fatal("known-unsent repair changed original event/source or saved native settings")
		}
		raceEvidence, _ := json.Marshal(state)
		t.Logf("native-monitor-steer-race %s", raceEvidence)
	}
	transcript, err := client.ReadThread(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range operation.Events {
		count := 0
		for _, entry := range transcript.Entries {
			if entry.Kind == "userMessage" && entry.ClientUserMessageID == event.ID {
				count++
				if entry.Text != event.Delivery.Envelope || event.Delivery.Receipt == nil || entry.TurnID != event.Delivery.Receipt.TurnID {
					t.Fatal("native event input differs from the durable envelope or receipt turn")
				}
			}
		}
		if mode == "cancel" {
			if count != 0 || event.Delivery.Receipt != nil {
				t.Fatal("canceled continuation activated lead")
			}
			continue
		}
		receipt := event.Delivery.Receipt
		if receipt == nil || receipt.ClientUserMessageID != event.ID || count != 1 || receipt.Steered != (mode == "busy") ||
			(mode == "busy" && receipt.TurnID != held.TurnID) || (mode != "busy" && receipt.TurnID == initial.TurnID) {
			t.Fatalf("event lacks one actual native activation: count=%d receipt=%+v", count, receipt)
		}
		turn, err := client.ReadTurnContext(ctx, thread, receipt.TurnID)
		if err != nil || turn.Model != settings.Model || turn.ReasoningEffort != settings.ReasoningEffort {
			t.Fatalf("actual lead continuation changed saved model/effort: %+v %v", turn, err)
		}
	}
	if mode != "missing-report" && mode != "native-restart" {
		before, _ := json.Marshal(operation.Events)
		call(operation.Launch.ThreadID, struct {
			Action string `json:"action"`
			monitoroperation.ReportRequest
		}{"report", report}, false)
		after, err := store.Get(ctx, operationID)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(after.Events)
		if !bytes.Equal(before, encoded) {
			t.Fatal("identical report changed original event or native receipt")
		}
	}
	rosterAfter, err := teamStore.Load("example", thread)
	if err != nil || !reflect.DeepEqual(rosterBefore, rosterAfter) {
		t.Fatal("monitor changed retained roster/settings", err)
	}
	if mode == "missing-report" && operation.Events[0].Report.Outcome != "incomplete" {
		t.Fatal("missing report inferred success")
	}
	if mode == "missing-report" {
		before, _ := json.Marshal(operation.Events)
		request := struct {
			Action string `json:"action"`
			monitoroperation.ResolutionRequest
		}{"resolve", monitoroperation.ResolutionRequest{OperationID: operationID, ResolutionID: strings.Repeat("b", 32),
			Evidence: []string{evidence}, Detail: "Exact fixture shell command exited; utility and lead are idle; original incomplete report retained"}}
		resolved := call(thread, request, false)
		retried := call(thread, request, false)
		if resolved.Resolution == nil || !reflect.DeepEqual(resolved.Resolution, retried.Resolution) || !reflect.DeepEqual(resolved.Source, operation.Source) {
			t.Fatal("evidence resolution changed identity or lost immutable acknowledgement")
		}
		after, _ := json.Marshal(resolved.Events)
		if !bytes.Equal(before, after) {
			t.Fatal("evidence resolution changed original outcomes or accepted receipts")
		}
		request.Detail = "conflicting evidence resolution"
		call(thread, request, true)
		inventory, err := store.Inventory(ctx, "example", thread)
		if err != nil || len(inventory.Unresolved) != 0 || len(inventory.Blockers) != 0 {
			t.Fatalf("exact evidence/native proof did not clear only the external-effects fence: %+v %v", inventory, err)
		}
		state, err := client.ReadTurnState(ctx, thread)
		if err != nil || state.LatestTurnID != transcript.LatestTurnID {
			t.Fatal("evidence resolution sent an unauthorized wake-up", err)
		}
	}
	if uncertain {
		starts, accepted, _, _, _, _, _ := barrier.snapshot()
		if starts != 1 || accepted != 1 {
			t.Fatal("receipt recovery submitted the same event more than once")
		}
	}
	operation, err = store.Get(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	events := make([]map[string]any, 0, len(operation.Events))
	for _, event := range operation.Events {
		events = append(events, map[string]any{
			"id": event.ID, "slot": event.Report.Slot, "outcome": event.Report.Outcome,
			"exitStatus": event.Report.ExitStatus, "conclusion": event.Report.Conclusion,
			"messageId": event.Delivery.MessageID, "context": event.Delivery.Context,
			"status": event.Delivery.Status, "receipt": event.Delivery.Receipt,
		})
	}
	retained, err := json.Marshal(map[string]any{
		"operationId": operation.ID, "owner": operation.Owner, "origin": initial, "source": operation.Source,
		"policy": map[string]any{"package": operation.UtilityPolicy.PackageRoot, "catalog": operation.UtilityPolicy.CatalogDigest,
			"model": operation.UtilityPolicy.Model, "effort": operation.UtilityPolicy.Effort},
		"utility": map[string]any{"projectId": operation.Launch.ProjectID, "threadId": operation.Launch.ThreadID,
			"launchReceipt": operation.Launch.Receipt},
		"events": events, "canceledAt": operation.CanceledAt, "resolution": operation.Resolution,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native-monitor-evidence %s", retained)
}
