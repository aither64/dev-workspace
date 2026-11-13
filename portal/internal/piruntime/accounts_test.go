package piruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestAccountBindingsFrozenAndRejectedBeforeNativeSetup(t *testing.T) {

	m, h, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	h.s.start = func(context.Context, workerMetadata, agentbackend.ExecutionHandle, workerConfig, func()) (worker, error) {
		called++
		return nil, errors.New("must not set up")
	}
	request := CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: "22222222-2222-4222-8222-222222222222", Settings: settingsFixture()}
	request.Settings.AccountRef = "wrong-account"
	if _, err := m.Create(ctx, runtime.Runtime, request); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if called != 0 {
		t.Fatal("account mismatch reached SDK setup")
	}
	m.config.AccountBindings = []AccountBinding{{Provider: "p3-fake", AccountRef: "changed-account"}}
	if _, err := m.Ensure(ctx, start); !errors.Is(err, ErrScope) {
		t.Fatal("changed frozen account map adopted", err)
	}
	path := filepath.Join(m.instance(runtime.Runtime), "requests", runtime.Runtime.RequestID, "runtime-config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(string(data), `"account_bindings":[{"provider":"p3-fake","account_ref":"fake-account"}]`, `"account_bindings":[]`, 1), strings.Replace(string(data), `"account_bindings":[{"provider":"p3-fake","account_ref":"fake-account"}]`, `"account_bindings":null`, 1)} {
		fixture := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(fixture, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readRuntimeConfiguration(fixture, h.s.launch, runtime.Runtime.Digest); err == nil {
			t.Fatal("missing map accepted")
		}
	}
}
