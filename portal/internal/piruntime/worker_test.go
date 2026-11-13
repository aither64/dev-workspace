package piruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

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
		case <-process.closed:
		default:
			_ = process.command.Process.Kill()
			<-process.closed
		}
	}()
	trace := filepath.Join(root, "sync-order.log")
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
