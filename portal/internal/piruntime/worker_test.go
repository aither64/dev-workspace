package piruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestRoleInstructionsCanonicalMappingRequestAndReplay(t *testing.T) {
	s, c, w, _ := policySupervisorFixture(t)
	role := "../SYSTEM.md\nžluťoučký literal\r\n"
	s.config.AccountBindings = []AccountBinding{{Provider: c.record.Settings.Provider, AccountRef: c.record.Settings.AccountRef}}
	legacy, err := json.Marshal(c.record)
	if err != nil || bytes.Contains(legacy, []byte(`"Instructions":`)) {
		t.Fatal("legacy mapping changed", err)
	}
	var decoded conversationRecord
	if err := strictDecode(legacy, &decoded, recordLimit); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(decoded)
	if !bytes.Equal(legacy, again) {
		t.Fatal("legacy roundtrip changed bytes")
	}
	for _, field := range []string{`"Instructions":""`, `"Instructions":null`, `"Instructions":1`, `"Instructions":false`, `"instructions":"role"`, `"Instructions":"a","Instructions":"b"`, `"Instructions":"nul\u0000text"`, `"Instructions":` + string(mustJSON(strings.Repeat("é", 2049)))} {
		data := append(append([]byte{}, legacy[:len(legacy)-1]...), []byte(","+field+"}")...)
		if err := strictDecode(data, &decoded, recordLimit); err == nil {
			t.Fatal("noncanonical instruction key accepted", field)
		}
	}
	c.record.Instructions = role
	data, _ := json.Marshal(c.record)
	if err := strictDecode(data, &decoded, recordLimit); err != nil || decoded.Instructions != role || validateConversationRecord(decoded, decoded.Conversation, c.root) != nil {
		t.Fatal("literal role rejected", err)
	}
	for _, schema := range []int{1, 3} {
		bad := c.record
		bad.Schema = schema
		data, _ := json.Marshal(bad)
		if strictDecode(data, &decoded, recordLimit) == nil {
			t.Fatal("instruction field accepted outside retained2", schema)
		}
	}
	utility := c.record
	utility.OwnerKind = "utility"
	data, _ = json.Marshal(utility)
	if strictDecode(data, &decoded, recordLimit) == nil {
		t.Fatal("utility instructions accepted")
	}
	if validateInstructions(string([]byte{0xff})) == nil || validateInstructions("nul\x00") == nil || validateInstructions(strings.Repeat("é", 2048)) != nil {
		t.Fatal("instruction UTF8/NUL/byte bounds changed")
	}
	request := CreateRequest{Session: c.record.Conversation.Session, Member: c.record.Conversation.Member, Incarnation: c.record.Conversation.Incarnation, CreationID: c.record.CreationID, Settings: c.record.InitialSettings, ToolPolicy: c.record.ToolPolicy}
	data, _ = json.Marshal(request)
	if bytes.Contains(data, []byte(`"Instructions":`)) {
		t.Fatal("empty request did not omit instructions")
	}
	var parsed CreateRequest
	for _, field := range []string{`"Instructions":null`, `"Instructions":""`, `"instructions":"role"`} {
		bad := append(append([]byte{}, data[:len(data)-1]...), []byte(","+field+"}")...)
		if strictDecode(bad, &parsed, recordLimit) == nil {
			t.Fatal("noncanonical request instructions accepted", field)
		}
	}
	for _, saved := range []string{"", role} {
		c.record.Instructions = saved
		if err := c.save(); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(filepath.Join(c.root, "conversation.json"))
		request.Instructions = role + "different"
		if _, err := s.create(context.Background(), request); !errors.Is(err, ErrConflict) {
			t.Fatal("replay retrofitted or replaced original instructions", err)
		}
		after, _ := os.ReadFile(filepath.Join(c.root, "conversation.json"))
		if !bytes.Equal(before, after) || w.applies != 0 {
			t.Fatal("mismatched replay changed mapping/native policy")
		}
	}
}

func mustJSON(value any) []byte { data, _ := json.Marshal(value); return data }

func TestRoleInstructionsAcknowledgementCodecAndExactLiteralDigest(t *testing.T) {
	if instructionsDigest("") != "" || instructionsDigest("é\n") != "edd3a863872a04239eb29ad4bc12fc892b3d4ae57cc7e786a3697816f8e141c2" {
		t.Fatal("digest is not exact original UTF8 bytes")
	}
	for _, raw := range []string{`{}`, `{"instructionsDigest":"` + strings.Repeat("a", 64) + `"}`} {
		var snapshot workerSnapshot
		if err := strictDecode([]byte(raw), &snapshot, recordLimit); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{`{"instructionsDigest":null}`, `{"instructionsDigest":""}`, `{"instructionsDigest":3}`, `{"InstructionsDigest":"` + strings.Repeat("a", 64) + `"}`, `{"instructionsDigest":"` + strings.Repeat("A", 64) + `"}`, `{"instructionsDigest":"short"}`, `{"instructionsDigest":"` + strings.Repeat("a", 64) + `","instructionsDigest":"` + strings.Repeat("a", 64) + `"}`} {
		var snapshot workerSnapshot
		if strictDecode([]byte(raw), &snapshot, recordLimit) == nil {
			t.Fatal("malformed application acknowledgement accepted", raw)
		}
	}
}

func TestRoleInstructionsCreationProofUsesFreshObservedDigestAndLegacyBytes(t *testing.T) {
	s, c, w, handle := policySupervisorFixture(t)
	if err := makePrivateDirectory(c.receipts.root); err != nil {
		t.Fatal(err)
	}
	request := conversationRequest{Handle: handle, CreationID: c.record.CreationID, HistoryRef: c.record.HistoryRef}
	legacy, err := s.verifyCreation(context.Background(), request)
	if err != nil || legacy.InstructionsDigest != "" {
		t.Fatal("legacy proof invented role evidence", legacy, err)
	}
	oldShape := struct {
		Conversation                       agentbackend.ConversationRef
		Generation, CreationID, HistoryRef string
		Persisted                          bool
		Bootstrap                          *SubmissionStatus
	}{legacy.Conversation, legacy.Generation, legacy.CreationID, legacy.HistoryRef, legacy.Persisted, legacy.Bootstrap}
	data, _ := json.Marshal(legacy)
	if !bytes.Equal(data, mustJSON(oldShape)) || bytes.Contains(data, []byte(`"InstructionsDigest":`)) {
		t.Fatal("legacy proof bytes changed", string(data))
	}
	c.record.Instructions = "é\noriginal role"
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(c.root, "conversation.json"))
	w.snapshot.InstructionsDigest = instructionsDigest(c.record.Instructions)
	proof, err := s.verifyCreation(context.Background(), request)
	if err != nil || proof.InstructionsDigest != w.snapshot.InstructionsDigest || !hexPattern.MatchString(proof.InstructionsDigest) {
		t.Fatal("verified fresh snapshot not projected", proof, err)
	}
	for _, fault := range []string{"missing", "mismatch", "replacement", "cold"} {
		c.worker = w
		w.snapshot.InstructionsDigest = instructionsDigest(c.record.Instructions)
		switch fault {
		case "missing":
			w.snapshot.InstructionsDigest = ""
		case "mismatch":
			w.snapshot.InstructionsDigest = strings.Repeat("f", 64)
		case "replacement":
			c.worker = &policyWorker{snapshot: workerSnapshot{NativeID: handle.Conversation.NativeID}}
		case "cold":
			c.worker = nil
		}
		if missing, err := s.verifyCreation(context.Background(), request); err == nil || missing.InstructionsDigest != "" {
			t.Fatal("unproved worker reused prior role evidence", fault, missing, err)
		}
	}
	after, _ := os.ReadFile(filepath.Join(c.root, "conversation.json"))
	entries, err := os.ReadDir(c.receipts.root)
	if !bytes.Equal(before, after) || err != nil || len(entries) != 0 || w.applies != 0 {
		t.Fatal("observational role proof changed original state", err)
	}
}

func TestPinnedSDKPipeWitnessReceiptAndLostAck(t *testing.T) {
	pi := os.Getenv("PI_PROBE_PACKAGE")
	if pi == "" {
		t.Skip("explicit pinned Pi package required")
	}
	wrapper, err := os.ReadFile(filepath.Join(pi, "bin", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	node := regexp.MustCompile(`/nix/store/[a-z0-9]{32}-nodejs-[^/\s"']+/bin/node`).FindString(string(wrapper))
	if node == "" {
		t.Fatal("package Node unavailable")
	}
	entry, err := filepath.Abs("../../../pi-runtime/test/fake-worker.mjs")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, name := range []string{"agent", "auth", "history", "repo"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	engine, ref, input, settings := receiptFixture(t)
	settings.Provider, settings.Model, settings.Thinking = "p3-fake", "p3-model", "off"
	settings.AccountRef = "fake-account"
	ref.Conversation.NativeID = "33333333-3333-4333-8333-333333333333"
	ref.Digest = SubmissionDigest(ref, input, settings)
	handle := agentbackend.ExecutionHandle{Conversation: ref.Conversation, Generation: "generation"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := startWorker(ctx, workerMetadata{Node: node, Entrypoint: entry, PiPackage: pi}, handle,
		workerConfig{NativeID: ref.Conversation.NativeID, EmptyProven: true, Cwd: filepath.Join(root, "repo"), AgentDir: filepath.Join(root, "agent"), AuthDirectory: filepath.Join(root, "auth"), HistoryDir: filepath.Join(root, "history"), PiPackage: pi, Settings: settings, AccountBindings: []AccountBinding{{Provider: settings.Provider, AccountRef: settings.AccountRef}}}, func() {})
	if err != nil {
		t.Fatal(err)
	}
	process := client.(*workerProcess)
	defer func() {
		select {
		case <-process.host.closed:
		default:
			_ = process.host.command.Process.Kill()
			<-process.host.closed
		}
	}()
	trace := filepath.Join(root, "agent", "sync-order.log")
	appendTrace := func(line string) {
		f, err := os.OpenFile(trace, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(line + "\n")
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	engine.checkpoint = func(point string) error { appendTrace("host-" + point); return nil }
	calls := 0
	dispatch := func(ctx context.Context) (nativeResult, error) {
		calls++
		var result nativeResult
		err := client.Call(ctx, "send", map[string]string{"message": input.Message}, &result)
		return result, err
	}
	abort := func() { _ = client.Call(ctx, "hold", struct{}{}, nil) }
	status, err := engine.send(ctx, ref, input, settings, handle.Generation, dispatch, abort)
	if err != nil || status.Phase != "Accepted" {
		t.Fatalf("native receipt: %+v %v", status, err)
	}
	appendTrace("ack")
	order, readErr := os.ReadFile(trace)
	if readErr != nil {
		t.Fatal(readErr)
	}
	expectedOrder := []string{"host-intent", "host-dispatch", "file-sync", "directory-sync", "native-sync-complete", "host-native", "host-witness", "host-receipt", "ack"}
	if !slices.Equal(strings.Fields(string(order)), expectedOrder) {
		t.Fatal("native/host durability ordering", string(order))
	}
	// The host ACK is constructed only after the independently persisted native
	// witness has been fsynced into this actual host receipt.
	committed, err := engine.lookup(ref)
	if err != nil || committed.Phase != "Accepted" || committed.Witness.EntryID == "" {
		t.Fatal(committed, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := engine.send(ctx, ref, input, settings, "new-generation", dispatch, abort); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("same-ID ACK retry redispatched")
	}
	var snapshot workerSnapshot
	for {
		if err := client.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
			t.Fatal(err)
		}
		if !snapshot.Busy {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	ref2 := ref
	ref2.OperationID = "44444444-4444-4444-8444-444444444444"
	ref2.Digest = SubmissionDigest(ref2, input, settings)
	engine.checkpoint = func(point string) error {
		if point == "receipt" {
			return errors.New("lost ACK")
		}
		return nil
	}
	if _, err := engine.send(ctx, ref2, input, settings, handle.Generation, dispatch, abort); err == nil {
		t.Fatal("lost ACK fault absent")
	}
	restarted := &receiptEngine{root: engine.root}
	recovered, err := restarted.send(ctx, ref2, input, settings, "replacement", dispatch, abort)
	if err != nil || recovered.Phase != "Accepted" || calls != 2 {
		t.Fatalf("lost ACK recovery %+v %v calls=%d", recovered, err, calls)
	}
	if recovered.Witness.EntryID == committed.Witness.EntryID {
		t.Fatal("repeated text was confused with earlier entry")
	}
	for {
		if err := client.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
			t.Fatal(err)
		}
		if !snapshot.Busy {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := client.Call(ctx, "unload", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	pid, _ := client.PID()
	if !processGroupEmpty(pid) {
		t.Fatal("worker group survived unload")
	}
}

func TestPinnedSDKSharedMemberPipe(t *testing.T) {
	pi := os.Getenv("PI_PROBE_PACKAGE")
	if pi == "" {
		t.Skip("explicit pinned Pi package required")
	}
	wrapper, err := os.ReadFile(filepath.Join(pi, "bin/pi"))
	if err != nil {
		t.Fatal(err)
	}
	node := regexp.MustCompile(`/nix/store/[a-z0-9]{32}-nodejs-[^/\s"']+/bin/node`).FindString(string(wrapper))
	entry, err := filepath.Abs("../../../pi-runtime/test/fake-worker.mjs")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err = os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	metadata := workerMetadata{Protocol: 2, Node: node, Entrypoint: entry, PiPackage: pi}
	host, err := startWorkerHost(metadata, repo, &hostAdmission{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-host.closed:
		default:
			_ = host.command.Process.Kill()
			<-host.closed
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	clients := make([]*workerProcess, 0, 2)
	for i, native := range []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"} {
		_, ref, _, settings := receiptFixture(t)
		ref.Conversation.NativeID = native
		ref.Conversation.Member = []string{"lead", "implementer0"}[i]
		settings.Provider, settings.AccountRef, settings.Model, settings.Thinking = "p3-fake", "fake-account", "p3-model", "off"
		memberRoot := filepath.Join(root, ref.Conversation.Member)
		for _, n := range []string{"agent", "auth", "history"} {
			if err = os.MkdirAll(filepath.Join(memberRoot, n), 0700); err != nil {
				t.Fatal(err)
			}
		}
		cfg := workerConfig{NativeID: native, EmptyProven: true, Cwd: repo, AgentDir: filepath.Join(memberRoot, "agent"), HistoryDir: filepath.Join(memberRoot, "history"), AuthDirectory: filepath.Join(memberRoot, "auth"), PiPackage: pi, Settings: settings, AccountBindings: []AccountBinding{{Provider: settings.Provider, AccountRef: settings.AccountRef}}}
		client, err := host.attach(ctx, retainedWorkerIdentity(agentbackend.ExecutionHandle{Conversation: ref.Conversation, Generation: strings.Repeat("a", 64)}), cfg, func() {})
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
	}
	a, as := clients[0].PID()
	b, bs := clients[1].PID()
	if a != b || as != bs {
		t.Fatal("members did not share physical host")
	}
	outcomes := make(chan error, 2)
	for i, c := range clients {
		go func(i int, c *workerProcess) {
			var result nativeResult
			err := c.Call(ctx, "send", map[string]string{"message": "member fixture", "inputOperationID": []string{"a", "b"}[i]}, &result)
			if err == nil && result.Outcome != "Accepted" {
				err = ErrHeld
			}
			outcomes <- err
		}(i, c)
	}
	for range clients {
		if err := <-outcomes; err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range clients {
		for {
			var snapshot workerSnapshot
			if err = c.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.NativeID != c.handle.nativeID() {
				t.Fatal("cross-member snapshot")
			}
			if !snapshot.Busy {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
	if err = clients[0].Call(ctx, "abort", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	if err = clients[0].Call(ctx, "unload", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.closed:
		t.Fatal("member detach stopped sibling host")
	default:
	}
	var sibling workerSnapshot
	if err = clients[1].Call(ctx, "snapshot", struct{}{}, &sibling); err != nil || sibling.NativeID != clients[1].handle.nativeID() {
		t.Fatal("sibling unavailable after detach", err)
	}
	if err = clients[1].Call(ctx, "unload", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	if err = clients[1].Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if !processGroupEmpty(a) {
		t.Fatal("last member did not exit host")
	}
}

func TestPinnedSDKSharedCallbacksAndConfigure(t *testing.T) {
	pi := os.Getenv("PI_PROBE_PACKAGE")
	if pi == "" {
		t.Skip("explicit pinned Pi package required")
	}
	raw, err := os.ReadFile(filepath.Join(pi, "bin/pi"))
	if err != nil {
		t.Fatal(err)
	}
	node := regexp.MustCompile(`/nix/store/[a-z0-9]{32}-nodejs-[^/\s"']+/bin/node`).FindString(string(raw))
	entry, _ := filepath.Abs("../../../pi-runtime/test/fake-worker.mjs")
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	_ = os.Mkdir(repo, 0700)
	host, err := startWorkerHost(workerMetadata{Protocol: 2, Node: node, Entrypoint: entry, PiPackage: pi}, repo, &hostAdmission{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-host.closed:
		default:
			_ = host.command.Process.Kill()
			<-host.closed
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	blocked, release := make(chan struct{}), make(chan struct{})
	var reached sync.Once
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	clients := []*workerProcess{}
	counts := []atomic.Int64{{}, {}}
	for i, member := range []string{"lead", "implementer0"} {
		_, ref, _, settings := receiptFixture(t)
		ref.Conversation.Member = member
		ref.Conversation.NativeID = []string{"33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"}[i]
		settings.Provider, settings.AccountRef, settings.Model, settings.Thinking = "p3-fake", "fake-account", "p3-model", "off"
		dir := filepath.Join(root, member)
		for _, n := range []string{"agent", "auth", "history"} {
			if err = os.MkdirAll(filepath.Join(dir, n), 0700); err != nil {
				t.Fatal(err)
			}
		}
		identity := retainedWorkerIdentity(agentbackend.ExecutionHandle{Conversation: ref.Conversation, Generation: strings.Repeat("a", 64)})
		subject := identity.subject()
		index := i
		cfg := workerConfig{NativeID: ref.Conversation.NativeID, EmptyProven: true, Cwd: repo, AgentDir: filepath.Join(dir, "agent"), HistoryDir: filepath.Join(dir, "history"), AuthDirectory: filepath.Join(dir, "auth"), PiPackage: pi, Settings: settings, AccountBindings: []AccountBinding{{Provider: "p3-fake", AccountRef: "fake-account"}}, HostTools: []string{"report_to_lead"}, CallbackSubject: &subject}
		cfg.onHostCall = func(callCtx context.Context, call nativeHostCall) (HostReply, error) {
			if call.InputOperationID != []string{"input-a", "input-b"}[index] {
				return HostReply{}, ErrScope
			}
			counts[index].Add(1)
			if index == 1 {
				reached.Do(func() { close(blocked) })
				select {
				case <-release:
				case <-callCtx.Done():
					return HostReply{}, callCtx.Err()
				}
			}
			return HostReply{Outcome: "result", Body: json.RawMessage(`{"ok":true}`)}, nil
		}
		client, err := host.attach(ctx, identity, cfg, func() {})
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
	}
	for i, c := range clients {
		var result nativeResult
		if err = c.Call(ctx, "send", map[string]string{"message": "callback fixture", "inputOperationID": []string{"input-a", "input-b"}[i]}, &result); err != nil || result.Outcome != "Accepted" {
			t.Fatal(result, err)
		}
	}
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for {
		var snapshot workerSnapshot
		if err = clients[0].Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
			t.Fatal(err)
		}
		if !snapshot.Busy {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	settings := settingsFixture()
	settings.Revision++
	settings.Model = "p3-other"
	var changed SettingsResult
	if err = clients[0].Call(ctx, "configure", settings, &changed); err != nil || changed.Outcome != "applied" {
		t.Fatal(changed, err)
	}
	if err = clients[0].Call(ctx, "abort", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	var peer workerSnapshot
	if err = clients[1].Call(ctx, "snapshot", struct{}{}, &peer); err != nil || !peer.Busy || peer.Model != "p3-model" {
		t.Fatal("sibling work/settings were changed", peer, err)
	}
	close(release)
	for _, c := range clients {
		for {
			var snapshot workerSnapshot
			if err = c.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
				t.Fatal(err)
			}
			if !snapshot.Busy {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
	if counts[0].Load() != 2 || counts[1].Load() != 2 {
		t.Fatal("callback routing counts", counts[0].Load(), counts[1].Load())
	}
	for _, c := range clients {
		if err = c.Call(ctx, "unload", struct{}{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err = clients[1].Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSharedTopologyStrictLegacyRecordBoundary(t *testing.T) {
	_, c, _, _ := policySupervisorFixture(t)
	legacy, err := json.Marshal(c.record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(legacy, []byte("WorkerTopology")) {
		t.Fatal("legacy topology shape changed")
	}
	c.record.WorkerTopology = "shared-session-v1"
	shared, err := json.Marshal(c.record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded conversationRecord
	if err = strictDecode(shared, &decoded, recordLimit); err != nil {
		t.Fatal(err)
	}
	typ := reflect.TypeOf(conversationRecord{})
	fields := []reflect.StructField{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Name != "WorkerTopology" {
			fields = append(fields, field)
		}
	}
	// The retained exact field layout with only the new optional field absent.
	oldReader := reflect.New(reflect.StructOf(fields)).Interface()
	if err = strictDecode(legacy, oldReader, recordLimit); err != nil {
		t.Fatal("retained shape refused", err)
	}
	if err = strictDecode(shared, oldReader, recordLimit); err == nil {
		t.Fatal("old strict reader accepted shared PID semantics")
	}
	for _, bad := range []string{`null`, `""`, `"other"`} {
		raw := bytes.Replace(shared, []byte(`"shared-session-v1"`), []byte(bad), 1)
		if strictDecode(raw, &decoded, recordLimit) == nil {
			t.Fatal("invalid topology accepted", bad)
		}
	}
	result, _ := json.Marshal(StopResult{Outcome: "stopped", VerifiedExit: true})
	if bytes.Contains(result, []byte("VerifiedDetach")) {
		t.Fatal("legacy stop shape changed")
	}
}

func TestReadMemberAdmissionOnlyUsesOriginalRecords(t *testing.T) {
	_, c, _, handle := policySupervisorFixture(t)
	m := &Manager{config: Config{UserStateRoot: t.TempDir()}}
	directory, err := stateDirectory(m.config.UserStateRoot, handle.Conversation.Session)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(directory, "members", handle.Conversation.Member, handle.Conversation.Incarnation)
	if err = writePrivate(filepath.Join(root, "conversation.json"), c.record); err != nil {
		t.Fatal(err)
	}
	ref := SubmissionRef{Conversation: handle.Conversation, OperationID: strings.Repeat("d", 32), Action: "send"}
	input := SendInput{Message: "original retained input"}
	ref.Digest = SubmissionDigest(ref, input, c.record.Settings)
	receipt := SubmissionStatus{Schema: 1, Ref: ref, Phase: "dispatching", Generation: handle.Generation, Settings: c.record.Settings, Input: input}
	engine := &receiptEngine{root: filepath.Join(root, "submissions")}
	if err = engine.save(receipt); err != nil {
		t.Fatal(err)
	}
	reserved := handle.Conversation
	reserved.NativeID = ""
	before, _ := os.ReadFile(filepath.Join(root, "conversation.json"))
	oldReceipt, _ := os.ReadFile(filepath.Join(root, "submissions", ref.OperationID+".json"))
	state, status, err := m.ReadMemberAdmission(context.Background(), reserved, c.record.CreationID, ref.OperationID)
	if err != nil || state.State != "retained" || state.Conversation != handle.Conversation || status.Phase != "Unknown" || status.Ref != ref {
		t.Fatal(state, status, err)
	}
	bad := reserved
	bad.NativeID = "wrong"
	if _, _, err = m.ReadMemberAdmission(context.Background(), bad, c.record.CreationID, ref.OperationID); err == nil {
		t.Fatal("wrong native identity read another mapping")
	}
	if _, _, err = m.ReadMemberAdmission(context.Background(), reserved, strings.Repeat("e", 64), ref.OperationID); err == nil {
		t.Fatal("wrong creation recovered mapping")
	}
	state, _, err = m.ReadMemberAdmission(context.Background(), reserved, c.record.CreationID, strings.Repeat("f", 32))
	if !errors.Is(err, os.ErrNotExist) || state.Conversation != handle.Conversation {
		t.Fatal("missing receipt fabricated admission", state, err)
	}
	afterRead, _ := os.ReadFile(filepath.Join(root, "submissions", ref.OperationID+".json"))
	if !bytes.Equal(oldReceipt, afterRead) {
		t.Fatal("read port rewrote dispatching receipt")
	}
	receipt.Phase = "Accepted"
	receipt.Witness = &NativeWitness{SessionID: handle.Conversation.NativeID, EntryID: "entry", EffectiveDigest: MessageDigest(input.Message)}
	if err = engine.save(receipt); err != nil {
		t.Fatal(err)
	}
	_, status, err = m.ReadMemberAdmission(context.Background(), reserved, c.record.CreationID, ref.OperationID)
	if err != nil || status.Phase != "Accepted" || status.Witness.EntryID != "entry" {
		t.Fatal(status, err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "conversation.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("read port changed mapping")
	}

}
