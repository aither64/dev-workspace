package piruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestRoleInstructionsManagerOriginalReceiptReopenAndPriorWorkerRefusal(t *testing.T) {
	prior := os.Getenv("PI_ROLE_PRIOR_SOURCE")
	if prior == "" {
		t.Skip("exact prior-R source snapshot required")
	}
	for path, want := range map[string]string{
		"pi-runtime/src/session.mjs":      "b97d64d654c619e2bfcf34e4bb23eb9e184d903be9032ab4782c2d424e74f04f",
		"pi-runtime/src/protocol.mjs":     "b69f9bb7d9cf62a7ef75e8c8b041427480f9c6aa662aeafffd6a2fda271cbecc",
		"pi-runtime/test/fake-worker.mjs": "8fedf32d3538f2cdd89324273e1ea715aedafce8b0d03460439eb0fee4e209af",
	} {
		data, err := os.ReadFile(filepath.Join(prior, path))
		sum := sha256.Sum256(data)
		if err != nil || hex.EncodeToString(sum[:]) != want {
			t.Fatal("actual prior-R JS source mismatch", path, err)
		}
	}
	m, host, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := sessionDirectory(start.Session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cwd, ".pi"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{".pi/APPEND_SYSTEM.md": "original append marker\n", "AGENTS.md": "owned AGENTS marker\n"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	policy := policyTarget("default", 1, []string{}, []string{})
	literal := "./role-source.md\nřádky é\r\nexact literal\n"
	request := CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: settingsFixture(), ToolPolicy: &policy, Instructions: literal}
	state, err := m.Create(ctx, runtime.Runtime, request)
	if err != nil {
		t.Fatal(state, err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	c, err := host.s.member(handle.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	firstPID, _ := c.worker.PID()
	if proof, err := m.VerifyCreation(ctx, handle, request.CreationID, state.HistoryRef, nil); err != nil || proof.InstructionsDigest != instructionsDigest(literal) {
		t.Fatal("application proof before goal", err)
	}
	input := SendInput{Message: "one original role goal\nexact user bytes"}
	ref := SubmissionRef{Conversation: state.Conversation, OperationID: strings.Repeat("a", 32), Action: "send"}
	ref.Digest = SubmissionDigest(ref, input, request.Settings)
	accepted, err := m.SendCheckedSubmission(ctx, handle, ref, input)
	if err != nil || accepted.Phase != "Accepted" || accepted.Ref != ref || accepted.Witness == nil {
		t.Fatal(accepted, err)
	}
	waitIdle(t, m, ctx, handle)
	receiptPath := filepath.Join(c.receipts.root, ref.OperationID+".json")
	before, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		var snapshot workerSnapshot
		if err := c.worker.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil || snapshot.InstructionsDigest != instructionsDigest(literal) {
			t.Fatal("same-worker role proof", err)
		}
		users, provedSystem := 0, false
		for _, raw := range snapshot.Entries {
			var entry struct {
				Message struct {
					Role     string
					Content  json.RawMessage
					Sections map[string]string
				}
			}
			if json.Unmarshal(raw, &entry) != nil {
				t.Fatal("native entry")
			}
			if entry.Message.Role == "user" {
				users++
				var blocks []struct{ Text string }
				if json.Unmarshal(entry.Message.Content, &blocks) != nil || len(blocks) != 1 || blocks[0].Text != input.Message {
					t.Fatal("original goal changed")
				}
			}
			if entry.Message.Role == "system" {
				var text string
				_ = json.Unmarshal(entry.Message.Content, &text)
				for _, section := range entry.Message.Sections {
					text += section
				}
				if strings.Contains(text, "You are a coding assistant") && strings.Contains(text, "owned AGENTS marker") && strings.Contains(text, "original append marker") && strings.Count(text, literal) == 1 {
					provedSystem = true
				}
			}
		}
		if users != 1 || !provedSystem || c.record.Instructions != literal {
			t.Fatal("native original/effective prompt inventory", users, provedSystem)
		}
		after, err := os.ReadFile(receiptPath)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("original receipt changed", err)
		}
		if original, err := m.Lookup(ctx, ref); err != nil || original.Ref != ref || original.Witness == nil || *original.Witness != *accepted.Witness {
			t.Fatal("original lookup changed", original, err)
		}
		if proof, err := m.VerifyCreation(ctx, handle, request.CreationID, state.HistoryRef, &ref); err != nil || proof.InstructionsDigest != instructionsDigest(literal) || proof.Bootstrap == nil || proof.Bootstrap.Ref != ref {
			t.Fatal("fresh creation proof lost original applied role", proof, err)
		}
	}
	check()
	if stopped, err := m.Unload(ctx, handle); err != nil || !stopped.VerifiedExit {
		t.Fatal(stopped, err)
	}
	reopened, err := NewManager(m.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load(ctx, handle); err != nil {
		t.Fatal("saved role fresh Load", err)
	}
	if nextPID, _ := c.worker.PID(); nextPID == firstPID || c.record.Instructions != literal || host.count != 1 {
		t.Fatal("reopen reused worker/started runtime/adopted new role")
	}
	check()
	changed := request
	changed.Instructions += "later catalog text"
	if _, err := reopened.Create(ctx, runtime.Runtime, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("replay replaced frozen role", err)
	}
	settings := c.record.Settings
	settings.Model = "p3-other"
	settings.Revision++
	if configured, err := reopened.Configure(ctx, handle, settings); err != nil || configured.Outcome != "applied" {
		t.Fatal(configured, err)
	}
	check()
	if proof, err := reopened.VerifyCreation(ctx, handle, request.CreationID, state.HistoryRef, &ref); err != nil || proof.Bootstrap == nil || proof.Bootstrap.Ref != ref || proof.InstructionsDigest != instructionsDigest(literal) {
		t.Fatal("original creation receipt proof", proof, err)
	}

	// Import the unmodified actual prior worker, recording only its owned PID.
	// Its real init response has no role ACK; no Goal/Send is issued on refusal.
	oldEntry := filepath.Join(prior, "pi-runtime/test/fake-worker.mjs")
	pidPath, wrapper := filepath.Join(t.TempDir(), "prior.pid"), filepath.Join(t.TempDir(), "prior-worker.mjs")
	code := "import {writeFileSync} from 'node:fs'; import {pathToFileURL} from 'node:url'; writeFileSync(" + string(mustJSON(pidPath)) + ",String(process.pid),{mode:0o600}); await import(pathToFileURL(" + string(mustJSON(oldEntry)) + ").href);\n"
	if err := os.WriteFile(wrapper, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	oldStart := host.s.start
	attempts := 0
	host.s.metadata.Entrypoint = wrapper
	host.s.start = func(ctx context.Context, metadata workerMetadata, handle agentbackend.ExecutionHandle, config workerConfig, changed func()) (worker, error) {
		attempts++
		return oldStart(ctx, metadata, handle, config, changed)
	}
	failed := request
	failed.Member, failed.Incarnation, failed.CreationID = "implementer1", strings.Repeat("d", 64), strings.Repeat("e", 64)
	if _, err := reopened.Create(ctx, runtime.Runtime, failed); !errors.Is(err, ErrHeld) {
		t.Fatal("prior JS admitted nonempty role", err)
	}
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatal(err)
	}
	var exit syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &exit, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatal("prior owned worker not reaped", pid, err)
	}
	failedRoot := filepath.Join(host.s.stateRoot, "members", failed.Member, failed.Incarnation)
	var held conversationRecord
	if err := readPrivate(filepath.Join(failedRoot, "conversation.json"), &held); err != nil || held.Instructions != literal || !held.Loaded || held.WorkerPID != 0 {
		t.Fatal("original failed load intent lost", held, err)
	}
	heldBytes, _ := os.ReadFile(filepath.Join(failedRoot, "conversation.json"))
	if _, err := reopened.Create(ctx, runtime.Runtime, failed); !errors.Is(err, ErrHeld) || attempts != 1 {
		t.Fatal("prior JS refusal replaced original worker", err, attempts)
	}
	if _, err := reopened.Load(ctx, agentbackend.ExecutionHandle{Conversation: held.Conversation, Generation: held.Generation}); !errors.Is(err, ErrHeld) || attempts != 1 {
		t.Fatal("held intent auto restored", err, attempts)
	}
	heldAfter, _ := os.ReadFile(filepath.Join(failedRoot, "conversation.json"))
	if !bytes.Equal(heldBytes, heldAfter) {
		t.Fatal("held original mapping repaired")
	}
	for _, name := range []string{"history", "submissions"} {
		entries, err := os.ReadDir(filepath.Join(failedRoot, name))
		if err != nil || len(entries) != 0 {
			t.Fatal("prior role refusal created native goal/receipt", name, err, entries)
		}
	}
	host.s.start, host.s.metadata.Entrypoint = oldStart, oldEntry
	legacy := request
	legacy.Member, legacy.Incarnation, legacy.CreationID, legacy.Instructions, legacy.ToolPolicy = "implementer2", strings.Repeat("f", 64), strings.Repeat("9", 64), "", nil
	legacyState, err := reopened.Create(ctx, runtime.Runtime, legacy)
	if err != nil {
		t.Fatal("prior worker legacy init regressed", err)
	}
	legacyC, err := host.s.member(legacyState.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	var legacySnapshot workerSnapshot
	if err := legacyC.worker.Call(ctx, "snapshot", struct{}{}, &legacySnapshot); err != nil || legacySnapshot.InstructionsDigest != "" {
		t.Fatal("legacy worker invented role proof", err)
	}
	if proof, err := reopened.VerifyCreation(ctx, agentbackend.ExecutionHandle{Conversation: legacyState.Conversation, Generation: legacyState.Generation}, legacy.CreationID, legacyState.HistoryRef, nil); err != nil || proof.InstructionsDigest != "" {
		t.Fatal("legacy creation proof invented role digest", proof, err)
	}
	legacy.Instructions, legacy.ToolPolicy = literal, &policy
	if _, err := reopened.Create(ctx, runtime.Runtime, legacy); err == nil {
		t.Fatal("legacy-empty worker retrofitted")
	}
	check()
	t.Log("one original Goal/Accepted/witness unchanged; observed role preserved through fresh worker/new Manager/Configure; actual prior-R JS refused before Goal, reaped and original intent held; prior empty legacy supported")
}

func TestNativeToolPolicyCreateApplyInspectReloadAndReplay(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	plan := ToolPolicyTarget{Mode: "plan", Revision: 1, Active: []string{"read"}, RegisteredCeiling: []string{"read", "write"}}
	plan.Digest = ToolPolicyDigest(plan)
	request := CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: settingsFixture(), ToolPolicy: &plan}
	state, err := m.Create(ctx, runtime.Runtime, request)
	if err != nil || state.Persisted {
		t.Fatal(state, err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	proof, err := m.InspectToolPolicy(ctx, handle)
	if err != nil || ValidateToolPolicyProof(proof, plan, handle.Generation) != nil {
		t.Fatal(proof, err)
	}
	c, _ := h.s.member(handle.Conversation)
	if c.record.Schema != 2 || c.record.PendingToolPolicy != nil || c.record.ToolPolicy == nil {
		t.Fatal(c.record)
	}
	if _, err := m.Create(ctx, runtime.Runtime, request); err != nil {
		t.Fatal("exact initial policy replay", err)
	}
	missing := request
	missing.ToolPolicy = nil
	if _, err := m.Create(ctx, runtime.Runtime, missing); !errors.Is(err, ErrConflict) {
		t.Fatal("nil policy replay accepted", err)
	}
	changed := plan
	changed.Active = []string{}
	changed.Digest = ToolPolicyDigest(changed)
	different := request
	different.ToolPolicy = &changed
	if _, err := m.Create(ctx, runtime.Runtime, different); !errors.Is(err, ErrConflict) {
		t.Fatal("changed initial policy replay", err)
	}
	path := filepath.Join(start.Session.Workspace, "work", start.Session.Slug, "owned-policy-result")
	send := func(id string) {
		t.Helper()
		result, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "one explicit policy input", ClientUserMessageID: id})
		if err != nil || result.Outcome != backend.Accepted {
			t.Fatal(result, err)
		}
		waitIdle(t, m, ctx, handle)
	}
	send(strings.Repeat("1", 32))
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Plan native write took effect", err)
	}
	def := plan
	def.Mode = "default"
	def.Revision = 2
	def.Active = []string{"read", "write"}
	def.Digest = ToolPolicyDigest(def)
	if _, err := m.ApplyToolPolicy(ctx, handle, 2, def); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong revision mutated policy", err)
	}
	narrow := def
	narrow.Active = []string{"read"}
	narrow.RegisteredCeiling = []string{"read"}
	narrow.Digest = ToolPolicyDigest(narrow)
	if _, err := m.ApplyToolPolicy(ctx, handle, 1, narrow); !errors.Is(err, ErrConflict) {
		t.Fatal("ceiling replacement accepted", err)
	}
	result, err := m.ApplyToolPolicy(ctx, handle, 1, def)
	if err != nil || result.Outcome != "applied" || ValidateToolPolicyProof(result.Proof, def, handle.Generation) != nil {
		t.Fatal(result, err)
	}
	if _, err := m.Create(ctx, runtime.Runtime, request); !errors.Is(err, ErrUnavailable) {
		t.Fatal("advanced policy Create replay", err)
	}
	send(strings.Repeat("2", 32))
	if data, err := os.ReadFile(path); err != nil || string(data) != "Default permits" {
		t.Fatal("Default approved native write", string(data), err)
	}
	back := plan
	back.Revision = 3
	if result, err := m.ApplyToolPolicy(ctx, handle, 2, back); err != nil || result.Outcome != "applied" {
		t.Fatal(result, err)
	}
	send(strings.Repeat("3", 32))
	if data, err := os.ReadFile(path); err != nil || string(data) != "Default permits" {
		t.Fatal("returned Plan overwrote native file", string(data), err)
	}
	if _, err := m.Create(ctx, runtime.Runtime, request); !errors.Is(err, ErrUnavailable) {
		t.Fatal("same-mode later revision replayed initial Create", err)
	}
	if stop, err := m.Unload(ctx, handle); err != nil || !stop.VerifiedExit {
		t.Fatal(stop, err)
	}
	if _, err := m.InspectToolPolicy(ctx, handle); !errors.Is(err, ErrUnavailable) {
		t.Fatal("Inspect cold policy loaded worker", err)
	}
	if _, err := m.Load(ctx, handle); err != nil {
		t.Fatal(err)
	}
	proof, err = m.InspectToolPolicy(ctx, handle)
	if err != nil || ValidateToolPolicyProof(proof, back, handle.Generation) != nil {
		t.Fatal("retained policy reload", proof, err)
	}
	if stop, err := m.Unload(ctx, handle); err != nil || !stop.VerifiedExit {
		t.Fatal(stop, err)
	}
}

func TestNativeToolPolicyInitialPendingNeverRestartsCreate(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	policy := ToolPolicyTarget{Mode: "plan", Revision: 1, Active: []string{}, RegisteredCeiling: []string{}}
	policy.Digest = ToolPolicyDigest(policy)
	request := CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: settingsFixture(), ToolPolicy: &policy}
	setups := 0
	h.s.start = func(context.Context, workerMetadata, agentbackend.ExecutionHandle, workerConfig, func()) (worker, error) {
		setups++
		return nil, ErrUnavailable
	}
	if _, err := m.Create(ctx, runtime.Runtime, request); err == nil {
		t.Fatal("missing initialization failure")
	}
	if _, err := m.Create(ctx, runtime.Runtime, request); !errors.Is(err, ErrHeld) {
		t.Fatal("pending initialization replayed", err)
	}
	if setups != 1 {
		t.Fatal("repeated native initialization", setups)
	}
}
