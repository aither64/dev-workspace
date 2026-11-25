//go:build pi_native_fixture

package piruntime

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Extend the existing shared-worker upgrade fixture. The predecessor runs the
// retained production Node/SDK/protocol/session/persistence graph; the supervisor
// is in process and its old RPC-owning sources must match the reviewed legacy
// source. Host terminal/cgroup evidence remains the existing synthetic fixture.
func NewP6TeamConflictNativeUpgradeFixture(t *testing.T, options P6NativeOptions) P6NativeFixture {
	t.Helper()
	f := newP6NativeUpgradeFixture(t, true, options)
	host := f.Manager.config.Host.(*upgradeFixtureHost)
	original, err := f.Manager.ReadStart(f.Context, f.Runtime.Session)
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{original.AppStorePath, os.Getenv("PI_USABILITY_PACKAGE")} {
		for _, capability := range []struct {
			name               string
			configured, owning func(string) (bool, error)
		}{
			{"identity", f.Manager.config.conversationIdentityContract, readConversationIdentityContract},
			{"checked input", f.Manager.config.checkedInputContract, readCheckedInputContract},
			{"input delivery", f.Manager.config.inputDeliveryContract, readInputDeliveryContract},
		} {
			declared, declaredErr := capability.owning(app)
			configured, configuredErr := capability.configured(app)
			if declaredErr != nil || configuredErr != nil || !declared || configured != declared {
				t.Fatalf("immutable fixture %s capability mismatch for %s: declared=%t configured=%t: %v %v", capability.name, app, declared, configured, declaredErr, configuredErr)
			}
		}
	}
	legacy, err := readWorkerMetadata(original.AppStorePath)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := readWorkerMetadata(os.Getenv("PI_USABILITY_PACKAGE"))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Protocol != 2 || candidate.Protocol != 2 {
		t.Fatal("fixture requires the existing shared worker protocol")
	}
	legacy = teamConflictFixtureWorker(t, legacy, options.Models, true)
	candidate = teamConflictFixtureWorker(t, candidate, options.Models, false)
	host.base.metadata = legacy
	host.base.s.metadata = legacy
	old := host.base.s
	if old.ref != f.Runtime {
		t.Fatal("fixture supervisor is not the original runtime")
	}
	wrapper := &recoveryFixtureHost{upgradeFixtureHost: host, beforeStop: func() {
		host.base.mu.Lock()
		current := host.base.s
		host.base.mu.Unlock()
		if current != old || old.ref != f.Runtime {
			t.Fatal("predecessor supervisor binding changed before Stop")
		}
		old.hostMu.Lock()
		physical := old.sharedHost
		old.hostMu.Unlock()
		if physical == nil || !physical.shared {
			t.Fatal("predecessor did not use shared physical worker")
		}
		old.mu.Lock()
		members := make([]*conversation, 0, len(old.members))
		for _, member := range old.members {
			members = append(members, member)
		}
		old.mu.Unlock()
		if len(members) != 2 {
			t.Fatal("shared predecessor does not own both retained members")
		}
		for _, member := range members {
			member.mu.Lock()
			record, owner := member.record, member.worker
			member.mu.Unlock()
			if counted, ok := owner.(*upgradeCountedWorker); ok {
				owner = counted.worker
			}
			process, ok := owner.(*workerProcess)
			if !ok || process.host != physical || !record.Loaded || record.Generation != f.Runtime.Generation || record.WorkerTopology != "shared-session-v1" {
				t.Fatal("retained member is not bound to the shared predecessor", record.Conversation.Member)
			}
			pid, start := process.PID()
			if pid <= 0 || start == "" || pid != physical.command.Process.Pid || start != physical.start || record.WorkerPID != pid || record.WorkerStart != start {
				t.Fatal("retained member physical worker identity changed", record.Conversation.Member)
			}
		}
		select {
		case <-physical.closed:
		default:
			if err := physical.command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			<-physical.closed
		}
		host.base.metadata = candidate
	}}
	f.Manager.config.Host = wrapper
	f.Manager.config.teamConflictRecoveryContract = TeamConflictRecoveryPackage
	f.Manager.config.teamConflictPredecessorContract = teamConflictPredecessorPackage
	// Existing fixture validation is synthetic; selected target capability and
	// original legacy eligibility above remain the real immutable metadata reads.
	f.Manager.config.validatePackage = func(app string) error {
		if app != os.Getenv("PI_USABILITY_PACKAGE") {
			return fmt.Errorf("unexpected candidate: %w", ErrScope)
		}
		return ValidatePackage(app)
	}
	return f
}

func teamConflictFixtureWorker(t *testing.T, metadata workerMetadata, models []P6NativeModel, conflict bool) workerMetadata {
	t.Helper()
	source, err := os.ReadFile("../../../pi-runtime/test/fake-worker.mjs")
	if err != nil {
		t.Fatal(err)
	}

	t.Helper()
	code := string(source)
	for _, name := range []string{"protocol", "persistence"} {
		path := filepath.Join(filepath.Dir(metadata.Entrypoint), name+".mjs")
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
		code = strings.ReplaceAll(code, "../src/"+name+".mjs", (&url.URL{Scheme: "file", Path: path}).String())
	}

	encoded, err := json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	begin := strings.Index(code, "  const fake = ai.fauxProvider(")
	finish := strings.Index(code, "  if (config.instructions === 'fixture:policy-child') {")
	if begin < 0 || finish < begin {
		t.Fatal("existing faux registration seam changed")
	}
	registration := `  const selectedModels = ` + string(encoded) + `;
  const fakes = new Map();
  for(const provider of new Set(selectedModels.map(m=>m.Provider))) {
    const ids=[...new Set(selectedModels.filter(m=>m.Provider===provider).map(m=>m.Model))];
    const faux=ai.fauxProvider({provider,models:ids.map(id=>({id,reasoning:true})),tokensPerSecond:1000000});
    for(const model of faux.models)model.thinkingLevelMap={off:'off',minimal:'minimal',low:'low',medium:'medium',high:'high',xhigh:'xhigh',max:'max'};
    fakes.set(provider,faux);runtime.registerNativeProvider(faux.provider);
  }
  await runtime.refresh({allowNetwork:false});
  const fake=fakes.get(config.settings.Provider);
  if(!fake)throw new Error('missing exact frozen faux provider');
  for(const expected of selectedModels){
    const faux=fakes.get(expected.Provider),model=runtime.getModel(expected.Provider,expected.Model);
    if(runtime.getProvider(expected.Provider)!==faux.provider || !model || model.api!==faux.api)throw new Error('faux route was not selected');
  }
  const stream=runtime.stream.bind(runtime),streamSimple=runtime.streamSimple.bind(runtime);
  const check=model=>{const faux=fakes.get(model.provider);if(!faux || model.api!==faux.api || !faux.models.some(m=>m.id===model.id))throw new Error('non-faux inference refused');};
  runtime.stream=(model,...args)=>{check(model);return stream(model,...args);};
  runtime.streamSimple=(model,...args)=>{check(model);return streamSimple(model,...args);};
  const alternate=ai.fauxProvider({provider:'fixture-unused-alternate',models:[{id:'unused'}]});
`
	code = code[:begin] + registration + code[finish:]
	code = strings.Replace(code, "// Source fixture only;", "globalThis.fetch=()=>{throw new Error('native fixture forbids network');};\n// Source fixture only;", 1)
	if conflict {
		marker := "  if (config.instructions === 'fixture:policy-child') {"
		replacement := `  if (config.callbackSubject?.Conversation?.Conversation?.Member === 'lead') {
    fake.setResponses([
      ai.fauxAssistantMessage('original seed answered'),
      ai.fauxAssistantMessage([ai.fauxToolCall('team_message', {message_id:'55555555-5555-4555-8555-555555555555',to:'reviewer0',message:'one conflicting original assignment'})], {stopReason:'toolUse'}),
      ai.fauxAssistantMessage('callback result observed'),
    ]);
  } else if (config.instructions === 'fixture:policy-child') {`
		if strings.Count(code, marker) != 1 {
			t.Fatal("fixture response seam changed")
		}
		code = strings.Replace(code, marker, replacement, 1)
	} else {
		// Recovery loads history without continuing the interrupted model request.
		// The one later synthetic input has no external tool effect.
		marker := "  if (config.instructions === 'fixture:policy-child') {"
		code = strings.Replace(code, marker, "  if (config.callbackSubject?.Conversation?.Conversation?.Member === 'lead') { fake.setResponses([ai.fauxAssistantMessage('one later input answered')]); } else if (config.instructions === 'fixture:policy-child') {", 1)
	}
	path := filepath.Join(t.TempDir(), "retained-faux-worker.mjs")
	if err := os.WriteFile(path, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	metadata.Entrypoint = path
	return metadata
}

func TestTeamConflictRetainedWrapperPreparation(t *testing.T) {
	app := os.Getenv("PI_CENTRAL_APP_ROOT")
	if app == "" {
		t.Skip("explicit retained package required")
	}
	metadata, err := readWorkerMetadata(app)
	if err != nil {
		t.Fatal(err)
	}
	for _, conflict := range []bool{false, true} {
		wrapper := teamConflictFixtureWorker(t, metadata, []P6NativeModel{{Provider: "openai", Model: "syntax-only", Thinking: "high"}}, conflict)
		if output, err := exec.Command(metadata.Node, "--check", wrapper.Entrypoint).CombinedOutput(); err != nil {
			t.Fatalf("wrapper syntax: %v\n%s", err, output)
		}
	}
}
