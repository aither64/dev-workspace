package piruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

const modelConfigurationFixture = `{"providers":{"openai":{"baseUrl":"http://127.0.0.1:2455/primary/v1","api":"openai-responses"}}}`

const physicalModelFixture = `{"id":"gpt-6.2-sol","name":"GPT-6.2-Sol","contextWindow":500000,"reasoning":true,"input":["text","image"],"samplingParams":{"parallel_tool_calls":false},"thinkingLevelMap":{"off":null,"minimal":null,"low":"low","medium":"medium","high":"high","xhigh":"xhigh","max":"max"}}`

func TestInstalledPhysicalModelConfiguration(t *testing.T) {
	data := strings.Replace(modelConfigurationFixture, `"api":"openai-responses"`, `"api":"openai-responses","modelOverrides":{"gpt-6.1-sol":{"samplingParams":{"parallel_tool_calls":false}}},"models":[`+physicalModelFixture+`]`, 1)
	app := t.TempDir()
	writeModelConfiguration(t, app, data)
	got, err := LoadInstalledModelConfiguration(app)
	if err != nil || len(got.Models) != 1 || got.Model != "gpt-6.1-sol" || got.Models[0].ID != "gpt-6.2-sol" {
		t.Fatal(got, err)
	}
	for name, invalid := range map[string]string{
		"duplicate":    strings.Replace(data, `"models":[`+physicalModelFixture+`]`, `"models":[`+physicalModelFixture+`,`+physicalModelFixture+`]`, 1),
		"overlap":      strings.Replace(data, `"id":"gpt-6.2-sol"`, `"id":"gpt-6.1-sol"`, 1),
		"effort clamp": strings.Replace(data, `,"xhigh":"xhigh"`, ``, 1),
		"ultra":        strings.Replace(data, `"max":"max"`, `"max":"max","ultra":"max"`, 1),
		"parallel":     strings.Replace(data, `"parallel_tool_calls":false},"thinkingLevelMap"`, `"parallel_tool_calls":true},"thinkingLevelMap"`, 1),
		"credential":   strings.Replace(data, `"contextWindow":500000`, `"contextWindow":500000,"apiKey":"forbidden"`, 1),
		"endpoint":     strings.Replace(data, `"contextWindow":500000`, `"contextWindow":500000,"baseUrl":"http://other.test/v1"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			writeModelConfiguration(t, app, invalid)
			if _, err := LoadInstalledModelConfiguration(app); !errors.Is(err, ErrScope) {
				t.Fatal("invalid physical model accepted", err)
			}
		})
	}
}

func writeModelConfiguration(t *testing.T, app, data string) string {
	t.Helper()
	path := filepath.Join(app, installedModelsPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	// Replacing this owned fixture is deliberate; the loader never writes it.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0444); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInstalledModelConfiguration(t *testing.T) {
	root := t.TempDir()
	if got, err := LoadInstalledModelConfiguration(root); err != nil || got.Path != "" {
		t.Fatal(got, err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("absent input wrote files")
	}
	for name, value := range map[string]string{
		"valid": modelConfigurationFixture,
		"https": strings.Replace(modelConfigurationFixture, "http://127.0.0.1:2455/primary/v1", "https://gateway.example.test/some/path", 1),
		"ipv6":  strings.Replace(modelConfigurationFixture, "127.0.0.1", "[::1]", 1),
	} {
		t.Run(name, func(t *testing.T) {
			app := t.TempDir()
			path := writeModelConfiguration(t, app, value)
			got, err := LoadInstalledModelConfiguration(app)
			if err != nil || got.Path != path {
				t.Fatal(got, err)
			}
			after, _ := os.ReadFile(path)
			if string(after) != value {
				t.Fatal("loader changed source")
			}
		})
	}
	bad := map[string]string{
		"null": `null`, "empty": `{}`, "providers_null": `{"providers":null}`,
		"provider_null": `{"providers":{"openai":null}}`, "wrong_type": `{"providers":[]}`,
		"missing_url":       `{"providers":{"openai":{"api":"openai-responses"}}}`,
		"missing_api":       strings.Replace(modelConfigurationFixture, `,"api":"openai-responses"`, "", 1),
		"null_url":          strings.Replace(modelConfigurationFixture, `"http://127.0.0.1:2455/primary/v1"`, `null`, 1),
		"api":               strings.Replace(modelConfigurationFixture, "openai-responses", "openai-completions", 1),
		"provider":          strings.Replace(modelConfigurationFixture, `"openai":`, `"other":`, 1),
		"case":              strings.Replace(modelConfigurationFixture, "baseUrl", "BaseUrl", 1),
		"duplicate":         strings.Replace(modelConfigurationFixture, `"api":`, `"api":"openai-responses","api":`, 1),
		"escaped_duplicate": strings.Replace(modelConfigurationFixture, `"api":`, `"a\u0070i":"openai-responses","api":`, 1),
		"trailing":          modelConfigurationFixture + `{}`, "oversized": strings.Repeat(" ", installedModelsLimit) + modelConfigurationFixture,
		"invalid_utf8": modelConfigurationFixture + string([]byte{0xff}),
	}
	for _, field := range []string{"apiKey", "headers", "authHeader", "oauth", "models", "modelOverrides", "command"} {
		bad[field] = strings.Replace(modelConfigurationFixture, `"api":`, `"`+field+`":{},"api":`, 1)
	}
	for name, endpoint := range map[string]string{"relative": "/v1", "scheme": "file:///tmp/models", "host": "https:///v1", "user": "https://user:secret@gateway/v1", "query": "https://gateway/v1?x=1", "empty_query": "https://gateway/v1?", "fragment": "https://gateway/v1#x", "control": "https://gateway/\n", "escaped_control": "https://gateway/%0a", "interpolation": "https://gateway/${TOKEN}", "backslash": `https://gateway\path`, "space": " https://gateway/v1"} {
		raw, _ := json.Marshal(endpoint)
		bad[name] = strings.Replace(modelConfigurationFixture, `"http://127.0.0.1:2455/primary/v1"`, string(raw), 1)
	}
	for name, value := range bad {
		t.Run(name, func(t *testing.T) {
			app := t.TempDir()
			path := writeModelConfiguration(t, app, value)
			before, _ := os.ReadFile(path)
			got, err := LoadInstalledModelConfiguration(app)
			if !errors.Is(err, ErrScope) || !reflect.DeepEqual(got, InstalledModelConfiguration{}) {
				t.Fatal("invalid input accepted", got, err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal rewrote source")
			}
		})
	}
	t.Run("nonregular", func(t *testing.T) {
		app := t.TempDir()
		path := writeModelConfiguration(t, app, modelConfigurationFixture)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadInstalledModelConfiguration(app); !errors.Is(err, ErrScope) {
			t.Fatal(err)
		}
	})
}

func TestSupervisorInstalledModelConfiguration(t *testing.T) {
	m, host, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	appA, appB := t.TempDir(), t.TempDir()
	pathA := writeModelConfiguration(t, appA, modelConfigurationFixture)
	writeModelConfiguration(t, appB, strings.Replace(modelConfigurationFixture, "2455", "2456", 1))
	profile := filepath.Join(t.TempDir(), "current-profile")
	if err := os.Symlink(appB, profile); err != nil {
		t.Fatal(err)
	}
	launch := host.s.launch
	launch.AppStorePath, launch.ProfilePath = appA, profile
	config := host.s.config
	config.AccountBindings = []AccountBinding{{Provider: "openai", AccountRef: "fixture-account"}}
	s, err := newSupervisor(launch, config, host.s.handshake, host.s.metadata)
	if err != nil {
		t.Fatal(err)
	}
	if s.modelsPath != pathA {
		t.Fatal("retained A followed current B", s.modelsPath)
	}
	called := 0
	s.start = func(_ context.Context, _ workerMetadata, handle agentbackend.ExecutionHandle, cfg workerConfig, _ func()) (worker, error) {
		called++
		if cfg.ModelsPath != pathA || cfg.AuthDirectory != config.AuthDirectory || cfg.AgentDir == filepath.Dir(pathA) {
			t.Fatal("actual load lost immutable/private/canonical paths", cfg)
		}
		return &snapshotWorker{snapshot: workerSnapshot{NativeID: handle.Conversation.NativeID, Model: cfg.Settings.Model, Provider: cfg.Settings.Provider, Thinking: cfg.Settings.Thinking, HistoryFile: filepath.Join(cfg.HistoryDir, "fixture.jsonl")}}, nil
	}
	request := CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: "22222222-2222-4222-8222-222222222222", Settings: settingsFixture()}
	request.Settings.Provider, request.Settings.AccountRef = "openai", "fixture-account"
	if _, err := s.create(ctx, request); err != nil || called != 1 {
		t.Fatal("actual supervisor load", err, called)
	}
	before, err := os.ReadFile(filepath.Join(s.stateRoot, "members", request.Member, request.Incarnation, "conversation.json"))
	if err != nil {
		t.Fatal(err)
	}
	request.Settings.AccountRef = "wrong-account"
	if _, err := s.create(ctx, request); !errors.Is(err, ErrUnavailable) || called != 1 {
		t.Fatal("unmatched account started worker", err, called)
	}
	after, _ := os.ReadFile(filepath.Join(s.stateRoot, "members", request.Member, request.Incarnation, "conversation.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("refusal rewrote mapping")
	}
	for _, invalid := range []bool{false, true} {
		fresh := config
		fresh.UserStateRoot = t.TempDir()
		if invalid {
			writeModelConfiguration(t, appA, `{"providers":null}`)
		} else {
			fresh.AccountBindings = []AccountBinding{{Provider: "other", AccountRef: "fixture-account"}}
		}
		if _, err := newSupervisor(launch, fresh, host.s.handshake, host.s.metadata); err == nil {
			t.Fatal("invalid input/binding accepted")
		}
		entries, _ := os.ReadDir(fresh.UserStateRoot)
		if len(entries) != 0 {
			t.Fatal("invalid startup wrote runtime state")
		}
	}
	if runtime.Runtime != host.s.ref {
		t.Fatal("fixture identity drift")
	}
}

func TestInstalledModelConfigurationParallelTools(t *testing.T) {
	valid := strings.Replace(modelConfigurationFixture, `"api":"openai-responses"`, `"api":"openai-responses","modelOverrides":{"gpt-6.1-sol":{"samplingParams":{"parallel_tool_calls":true}}}`, 1)
	for _, value := range []string{`true`, `false`} {
		t.Run("boolean_"+value, func(t *testing.T) {
			root := t.TempDir()
			path := writeModelConfiguration(t, root, strings.Replace(valid, `"parallel_tool_calls":true`, `"parallel_tool_calls":`+value, 1))
			got, err := LoadInstalledModelConfiguration(root)
			if err != nil || got.Model != "gpt-6.1-sol" || got.Path != path {
				t.Fatal("exact native model override", got, err)
			}
		})
	}
	bad := map[string]string{}
	for _, value := range []string{`"true"`, `"false"`, `null`, `1`} {
		bad[value] = strings.Replace(valid, `"parallel_tool_calls":true`, `"parallel_tool_calls":`+value, 1)
	}
	for name, override := range map[string]string{
		"null_overrides": `null`, "empty_overrides": `{}`,
		"wildcard":          `{"gpt-*":{"samplingParams":{"parallel_tool_calls":true}}}`,
		"unknown_model_key": `{"gpt-6.1-sol":{"other":{},"samplingParams":{"parallel_tool_calls":true}}}`,
		"two_models":        `{"gpt-6.1-sol":{"samplingParams":{"parallel_tool_calls":true}},"other":{"samplingParams":{"parallel_tool_calls":true}}}`,
		"missing_sampling":  `{"gpt-6.1-sol":{}}`, "null_sampling": `{"gpt-6.1-sol":{"samplingParams":null}}`,
		"missing_flag":       `{"gpt-6.1-sol":{"samplingParams":{}}}`,
		"arbitrary_sampling": `{"gpt-6.1-sol":{"samplingParams":{"temperature":1,"parallel_tool_calls":true}}}`,
		"credential":         `{"gpt-6.1-sol":{"apiKey":"forbidden","samplingParams":{"parallel_tool_calls":true}}}`,
		"headers":            `{"gpt-6.1-sol":{"headers":{},"samplingParams":{"parallel_tool_calls":true}}}`,
		"duplicate":          `{"gpt-6.1-sol":{"samplingParams":{"parallel_tool_calls":true,"parallel_tool_calls":true}}}`,
	} {
		bad[name] = strings.Replace(modelConfigurationFixture, `"api":"openai-responses"`, `"api":"openai-responses","modelOverrides":`+override, 1)
	}
	for name, data := range bad {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := writeModelConfiguration(t, root, data)
			before, _ := os.ReadFile(path)
			if _, err := LoadInstalledModelConfiguration(root); !errors.Is(err, ErrScope) {
				t.Fatal("unsupported model override accepted", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal changed model input")
			}
		})
	}
}
