package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/inputdelivery"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

type localDeliveryRuntime struct {
	*piCreationRuntimeFixture
	selected                          piruntime.InputSelection
	reads, binds, dispatches, lookups int
	original                          piruntime.RuntimeRef
	live                              bool
	beforeNative                      func(piruntime.QueuedInputRequest)
	lookupResult                      piruntime.QueuedInputLookupResult
	lookupError                       error
	afterRead                         func(int)
	preflights, verifies              int
	preflightError                    error
	afterPreflight                    func()
	nativeError                       error
}

func TestDurablePortalShutdownUnderExclusiveTransitionPreservesWAL(t *testing.T) {
	s, f := localDeliveryFixture(t)
	defer s.Close()
	ctx := context.Background()
	ref := f.selected.Handle.Conversation
	code, data := localDeliveryHTTP(t, s, ref, "GET", "deliveries", nil)
	if code != 200 {
		t.Fatal(code, string(data))
	}
	var metadata backend.DeliveryCollection
	json.Unmarshal(data, &metadata)
	request := backend.DeliveryRequest{Version: 1, ClientUserMessageID: "11111111-1111-4111-8111-111111111111", Message: "retained across shutdown", Delivery: "steer", Selection: metadata.Selection}
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", request); code != 202 {
		t.Fatal(code, string(data))
	}
	path := inputdelivery.Path(s.config.UserStateRoot, s.config.Workspace)
	snapshot := func() map[string]string {
		out := map[string]string{}
		names, e := filepath.Glob(path + "*")
		if e != nil {
			t.Fatal(e)
		}
		for _, name := range names {
			data, e := os.ReadFile(name)
			if e != nil {
				t.Fatal(e)
			}
			info, e := os.Stat(name)
			if e != nil {
				t.Fatal(e)
			}
			out[filepath.Base(name)] = fmt.Sprint(info.Mode()) + string(data)
		}
		return out
	}
	before := snapshot()
	fd, err := unix.Open(s.config.TransitionLock, unix.O_RDWR|unix.O_CLOEXEC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.closeInputDelivery()
	if time.Since(start) > 750*time.Millisecond {
		t.Fatal("shutdown waited for exclusive transition")
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatal("EX-held close changed retained SQLite files")
	}
	if err = unix.Flock(fd, unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	next, err := inputdelivery.Open(ctx, s.config.UserStateRoot, s.config.Workspace, s.inputDeliveryDatabaseGate)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close(ctx)
	row, err := next.Get(ctx, metadata.Scope, request.ClientUserMessageID)
	if err != nil || row.Status.State != "queued" || f.binds != 0 || f.dispatches != 0 {
		t.Fatal(row, err)
	}
}

func (f *localDeliveryRuntime) ReadInputSelection(ctx context.Context, h agentbackend.ExecutionHandle) (piruntime.InputSelection, error) {
	f.reads++
	if h != f.selected.Handle {
		return piruntime.InputSelection{}, piruntime.ErrScope
	}
	observed := f.selected
	if f.afterRead != nil {
		f.afterRead(f.reads)
	}
	return observed, ctx.Err()
}
func (f *localDeliveryRuntime) ValidateInputRuntimeContinuation(ctx context.Context, original, current piruntime.RuntimeRef) error {
	expected := f.original
	if expected == (piruntime.RuntimeRef{}) {
		expected = f.selected.Runtime
	}
	if original != expected || current != f.selected.Runtime {
		return piruntime.ErrScope
	}
	return ctx.Err()
}
func (f *localDeliveryRuntime) Bind(_ context.Context, h agentbackend.ExecutionHandle) (backend.Operations, error) {
	f.binds++
	if f.live && h == f.selected.Handle {
		return backend.Operations{Verify: f}, nil
	}
	return backend.Operations{}, piruntime.ErrUnavailable
}
func (f *localDeliveryRuntime) Verify(ctx context.Context, id backend.Identity) error {
	f.verifies++
	if !f.live || id != piruntime.BackendIdentity(f.selected.Handle) {
		return piruntime.ErrScope
	}
	return ctx.Err()
}
func (f *localDeliveryRuntime) PreflightQueuedInput(ctx context.Context, h agentbackend.ExecutionHandle) error {
	f.preflights++
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.preflightError != nil {
		return f.preflightError
	}
	if _, err := f.Bind(ctx, h); err != nil {
		return err
	}
	if f.afterPreflight != nil {
		f.afterPreflight()
	}
	return ctx.Err()
}
func (f *localDeliveryRuntime) DispatchQueuedInput(_ context.Context, request piruntime.QueuedInputRequest) (piruntime.QueuedInputResult, error) {
	f.dispatches++
	if f.beforeNative != nil {
		f.beforeNative(request)
	}
	if f.nativeError != nil {
		return piruntime.QueuedInputResult{}, f.nativeError
	}
	if f.live {
		return piruntime.QueuedInputResult{Status: piruntime.SubmissionStatus{Schema: 1, Ref: request.Ref, Input: request.Input, Settings: request.Selection.Settings, Phase: "Accepted", Witness: &piruntime.NativeWitness{SessionID: request.Ref.Conversation.NativeID, EntryID: "entry", EffectiveDigest: piruntime.MessageDigest(request.Input.Message)}}}, nil
	}
	return piruntime.QueuedInputResult{}, piruntime.ErrUnavailable
}
func (f *localDeliveryRuntime) LookupQueuedInput(context.Context, piruntime.SubmissionRef) (piruntime.QueuedInputLookupResult, error) {
	f.lookups++
	if f.lookupResult.Status.Phase != "" || f.lookupError != nil {
		return f.lookupResult, f.lookupError
	}
	return piruntime.QueuedInputLookupResult{Status: piruntime.SubmissionStatus{Phase: "Unknown"}}, nil
}

func TestDurablePortalNegativeAdmissionProofSurvivesRestartAndLateOriginalPOST(t *testing.T) {
	s, f := localDeliveryFixture(t)
	defer s.Close()
	ref := f.selected.Handle.Conversation
	code, data := localDeliveryHTTP(t, s, ref, "GET", "deliveries", nil)
	if code != 200 {
		t.Fatal(code, string(data))
	}
	var metadata backend.DeliveryCollection
	json.Unmarshal(data, &metadata)
	r := backend.DeliveryRequest{Version: 1, ClientUserMessageID: "11111111-1111-4111-8111-111111111111", Message: "preserved draft", Delivery: "steer", Selection: metadata.Selection}
	f.selected.Settings.Revision++
	code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", r)
	var proof struct {
		NotQueued bool                           `json:"notQueued"`
		Proof     backend.DeliveryNotQueuedProof `json:"proof"`
	}
	if e := json.Unmarshal(data, &proof); code != 409 || e != nil || !proof.NotQueued || proof.Proof.Scope != metadata.Scope || !inputdelivery.SameRequest(proof.Proof.Request, r) {
		t.Fatal(code, string(data), e)
	}
	s.inputDeliveryStore.Close(context.Background())
	var err error
	s.inputDeliveryStore, err = inputdelivery.Open(context.Background(), s.config.UserStateRoot, s.config.Workspace, s.inputDeliveryDatabaseGate)
	if err != nil {
		t.Fatal(err)
	}
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", r); code != 409 || !strings.Contains(string(data), `"notQueued":true`) {
		t.Fatal(code, string(data))
	}
	changed := r
	changed.Message = "changed"
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", changed); code != 409 || strings.Contains(string(data), `"notQueued":true`) {
		t.Fatal(code, string(data))
	}
	code, data = localDeliveryHTTP(t, s, ref, "GET", "deliveries", nil)
	json.Unmarshal(data, &metadata)
	newRequest := r
	newRequest.ClientUserMessageID = "22222222-2222-4222-8222-222222222222"
	newRequest.Selection = metadata.Selection
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", newRequest); code != 202 {
		t.Fatal(code, string(data))
	}
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", r); code != 409 || !strings.Contains(string(data), `"notQueued":true`) {
		t.Fatal(code, string(data))
	}
	rows, err := s.inputDeliveryStore.List(context.Background(), metadata.Scope)
	if err != nil || len(rows) != 1 || rows[0].Status.ClientUserMessageID != newRequest.ClientUserMessageID || f.dispatches != 0 || f.binds != 0 {
		t.Fatal(rows, err)
	}
}

func TestDurablePortalSuccessorClaimsFreshExecutionAndPreservesAdmission(t *testing.T) {
	s, f := localDeliveryFixture(t)
	defer s.Close()
	ctx := context.Background()
	ref := f.selected.Handle.Conversation
	code, data := localDeliveryHTTP(t, s, ref, "GET", "deliveries", nil)
	if code != 200 {
		t.Fatal(code, string(data))
	}
	var metadata backend.DeliveryCollection
	json.Unmarshal(data, &metadata)
	r := backend.DeliveryRequest{Version: 1, ClientUserMessageID: "11111111-1111-4111-8111-111111111111", Message: "original", Delivery: "steer", Selection: metadata.Selection}
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", r); code != 202 {
		t.Fatal(code, string(data))
	}
	row, err := s.inputDeliveryStore.Get(ctx, metadata.Scope, r.ClientUserMessageID)
	if err != nil {
		t.Fatal(err)
	}
	original := inputdelivery.Digest(row.Admission)
	f.original = f.selected.Runtime
	bindingStore := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
	binding, err := bindingStore.Load(ref.Session.Slug)
	if err != nil {
		t.Fatal(err)
	}
	binding.Revision++
	body, _ := json.Marshal(binding)
	bindingPath := filepath.Join(s.config.UserStateRoot, "session-backends", fmt.Sprintf("%x", sha256.Sum256([]byte(s.config.Workspace))), ref.Session.Slug+".json")
	if err = os.WriteFile(bindingPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, ref.Session.Slug, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	authority.BindingRevision = binding.Revision
	authority.Generation = strings.Repeat("9", 64)
	piTeamAuthorityFixture(t, s, authority)
	f.selected.Runtime.Generation = authority.Generation
	f.selected.Runtime.RequestID = strings.Repeat("8", 64)
	f.selected.Handle.Generation = authority.Generation
	f.runtime = f.selected.Runtime
	f.live = true
	f.beforeNative = func(request piruntime.QueuedInputRequest) {
		claimed, e := s.inputDeliveryStore.Get(ctx, metadata.Scope, r.ClientUserMessageID)
		if e != nil || claimed.Status.State != "dispatching" || claimed.DispatchRuntime != f.selected.Runtime || claimed.DispatchHandle != f.selected.Handle || claimed.DispatchGeneration != authority.Generation || inputdelivery.Digest(claimed.Admission) != original || request.OriginalRuntime != f.original || request.Ref != row.Admission.Ref || request.Input != row.Admission.Input {
			t.Fatal("incorrect durable execution claim", claimed, request, e)
		}
	}
	driver := piDeliveryDriver{s}
	result, err := driver.Dispatch(ctx, row, func(execution inputdelivery.ExecutionEvidence) error {
		row.Status.State = "dispatching"
		row.DispatchAtMS = time.Now().UnixMilli()
		row.DispatchRuntime = execution.Runtime
		row.DispatchHandle = execution.Handle
		row.DispatchGeneration = execution.Handle.Generation
		won, e := s.inputDeliveryStore.Update(ctx, row, "queued")
		if e == nil && !won {
			return inputdelivery.ErrConflict
		}
		return e
	})
	if err != nil || result.State != "sent" || f.dispatches != 1 {
		t.Fatal(result, err, f.dispatches)
	}
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", r); code != 202 {
		t.Fatal("immutable original recovery", code, string(data))
	}
}

func TestDurablePortalLookupRequiresExactRetirementAndPreservesSafeRefusal(t *testing.T) {
	s, f := localDeliveryFixture(t)
	defer s.Close()
	ctx := context.Background()
	for _, reason := range []string{"Settings or mode changed. Review them before sending a new message.", "The worker could not be verified. Check the conversation before sending a new message.", "The worker could not accept this message. Check the conversation before sending a new message."} {
		if queuedRefusalMessage(reason) != reason {
			t.Fatal(reason)
		}
	}
	if strings.Contains(queuedRefusalMessage("secret arbitrary error"), "secret") {
		t.Fatal("unsafe refusal exposed")
	}
	current, err := s.piReadySessionIdentityLocked(f.selected.Handle.Conversation.Session)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := s.projectPiConversationAdmission(current, f.selected.Handle.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	selection := portalSelection(admission, f.selected)
	ref := piruntime.SubmissionRef{Conversation: f.selected.Handle.Conversation, OperationID: "11111111-1111-4111-8111-111111111111", Action: "send"}
	input := piruntime.SendInput{Message: "original", ActionContext: "pi-delivery-v1:steer"}
	ref.Digest = piruntime.SubmissionDigest(ref, input, selection.Settings)
	a := inputdelivery.Admission{Version: 1, Scope: piWorkflowNamespace(ref.Conversation), Request: backend.DeliveryRequest{Version: 1, ClientUserMessageID: ref.OperationID, Message: input.Message, Delivery: "steer", Selection: inputdelivery.Digest(selection)}, Wire: input.Message, Selection: selection, Ref: ref, Input: input}
	status := piruntime.SubmissionStatus{Schema: 1, Ref: ref, Input: input, Settings: selection.Settings, Phase: "Unknown"}
	for _, mutation := range []string{"exact", "missing", "ref", "input", "phase"} {
		f.lookupResult = piruntime.QueuedInputLookupResult{Status: status, RetiredUnknown: true}
		switch mutation {
		case "missing":
			f.lookupResult.Status = piruntime.SubmissionStatus{Phase: "Unknown"}
		case "ref":
			f.lookupResult.Status.Ref.OperationID = "22222222-2222-4222-8222-222222222222"
		case "input":
			f.lookupResult.Status.Input.Message = "changed"
		case "phase":
			f.lookupResult.Status.Phase = "Pending"
		}
		result, e := (piDeliveryDriver{s}).Lookup(ctx, inputdelivery.Record{Admission: a})
		if mutation == "exact" {
			if e != nil || !result.RetiredUnknown || result.State != "unknown" {
				t.Fatal(result, e)
			}
		} else if e == nil || result.RetiredUnknown {
			t.Fatal("unproved retirement", mutation, result, e)
		}
	}
	if !reflect.DeepEqual(a.Ref, ref) {
		t.Fatal("lookup rewrote original")
	}
}

func localDeliveryFixture(t *testing.T) (*Server, *localDeliveryRuntime) {
	t.Helper()
	s, creation, native := newPiCreationFixtureForSlug(t, "2026-11-23-local-delivery")
	if err := s.initializePiCreation(creation); err != nil {
		t.Fatal(err)
	}
	piTeamPackageFixture(t, s)
	f := &localDeliveryRuntime{piCreationRuntimeFixture: native, selected: piruntime.InputSelection{Runtime: native.runtime, Handle: agentbackend.ExecutionHandle{Conversation: native.state.Conversation, Generation: native.state.Generation}, Settings: native.state.Settings, InstructionsDigest: strings.Repeat("a", 64), CheckedInput: true}}
	s.config.PiCreation.Runtime = f
	store, err := inputdelivery.Open(context.Background(), s.config.UserStateRoot, s.config.Workspace, s.inputDeliveryDatabaseGate)
	if err != nil {
		t.Fatal(err)
	}
	s.inputDeliveryStore = store
	return s, f
}

func TestDurablePortalAdmissionPinsExactOrderedUploadsAndRechecksSelection(t *testing.T) {
	for _, changeDuringPin := range []bool{false, true} {
		t.Run(fmt.Sprint(changeDuringPin), func(t *testing.T) {
			s, f := localDeliveryFixture(t)
			defer s.Close()
			ctx := context.Background()
			ref := f.selected.Handle.Conversation
			s.uploadStore.MinFreeBytes = 0
			current, err := s.piReadySessionIdentityLocked(ref.Session)
			if err != nil {
				t.Fatal(err)
			}
			files, err := s.piSessionUploads(ctx, current, ref, false)
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{}
			for i, name := range []string{"first.txt", "second.txt"} {
				content := []byte(name)
				file, e := files.Create(ctx, conversation.UploadRequest{ClientID: fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1), Name: name, Size: int64(len(content))})
				if e != nil {
					t.Fatal(e)
				}
				sum := sha256.Sum256(content)
				if _, e = files.Append(ctx, file.ID, 0, fmt.Sprintf("%x", sum), bytes.NewReader(content)); e != nil {
					t.Fatal(e)
				}
				if _, e = files.Complete(ctx, file.ID); e != nil {
					t.Fatal(e)
				}
				ids = append(ids, file.ID)
			}
			code, data := localDeliveryHTTP(t, s, ref, "GET", "deliveries", nil)
			if code != 200 {
				t.Fatal(code, string(data))
			}
			var metadata backend.DeliveryCollection
			json.Unmarshal(data, &metadata)
			request := backend.DeliveryRequest{Version: 1, ClientUserMessageID: "33333333-3333-4333-8333-333333333333", Message: "inspect files", AttachmentIDs: []string{ids[1], ids[0]}, Delivery: "followUp", Selection: metadata.Selection}
			if changeDuringPin {
				f.afterRead = func(n int) {
					if n == 2 {
						f.selected.Settings.Revision++
					}
				}
			}
			lock, err := session.LockRuntimeExclusiveContext(ctx, s.config.AuthorityDir, ref.Session.Slug)
			if err != nil {
				t.Fatal(err)
			}
			code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", request)
			lock.Close()
			if changeDuringPin {
				if code != 409 || !strings.Contains(string(data), `"notQueued":true`) {
					t.Fatal(code, string(data))
				}
			} else {
				if code != 202 {
					t.Fatal(code, string(data))
				}
				row, e := s.inputDeliveryStore.Get(ctx, metadata.Scope, request.ClientUserMessageID)
				if e != nil || row.Admission.UploadScope != files.ScopeID || !reflect.DeepEqual(row.Admission.Request.AttachmentIDs, request.AttachmentIDs) || row.Admission.Input.Message != row.Admission.Wire || strings.Index(row.Admission.Wire, "second.txt") > strings.Index(row.Admission.Wire, "first.txt") {
					t.Fatal(row, e)
				}
			}
			// Both admitted and conservatively pinned rejected requests retain their
			// exact prepared bytes; neither native locks nor a response loss retargets.
			wire, err := files.Prepare(ctx, "send", request.ClientUserMessageID, request.Message, request.AttachmentIDs)
			if err != nil || wire == request.Message {
				t.Fatal(wire, err)
			}
			if _, err = files.Prepare(ctx, "send", request.ClientUserMessageID, request.Message, ids); err == nil {
				t.Fatal("changed file order reused pin")
			}
			if f.binds != 0 || f.dispatches != 0 || f.lookups != 0 {
				t.Fatal("upload admission invoked native")
			}
		})
	}
}
func localDeliveryHTTP(t *testing.T, s *Server, ref agentbackend.ConversationRef, method, operation string, input any, generation ...string) (int, []byte) {
	t.Helper()
	body := ""
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = string(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(method, "/codex/conversations/"+ref.Session.Slug+"/"+operation, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Origin", s.config.BaseURL)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Workspace-Conversation-Scope", piWorkflowNamespace(ref))
	if len(generation) > 0 {
		r.Header.Set("X-Workspace-Pi-Generation", generation[0])
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func TestDurablePortalStableScopeRecoversAcrossOldBrowserGeneration(t *testing.T) {
	s, f := localDeliveryFixture(t)
	defer s.Close()
	ref := f.selected.Handle.Conversation
	original := f.selected.Handle.Generation
	code, data := localDeliveryHTTP(t, s, ref, "GET", "deliveries", nil, original)
	if code != 200 {
		t.Fatal(code, string(data))
	}
	var metadata backend.DeliveryCollection
	json.Unmarshal(data, &metadata)
	request := backend.DeliveryRequest{Version: 1, ClientUserMessageID: "11111111111111111111111111111111", Message: "original", Delivery: "steer", Selection: metadata.Selection}
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", request, original); code != 202 {
		t.Fatal(code, string(data))
	}
	authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, ref.Session.Slug, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	authority.Generation = strings.Repeat("9", 64)
	piTeamAuthorityFixture(t, s, authority)
	f.selected.Handle.Generation = authority.Generation
	f.selected.Runtime.Generation = authority.Generation
	for _, operation := range []string{"deliveries", "deliveries/" + request.ClientUserMessageID} {
		if code, data = localDeliveryHTTP(t, s, ref, "GET", operation, nil, original); code != 200 {
			t.Fatal(operation, code, string(data))
		}
	}
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", request, original); code != 202 {
		t.Fatal("original-ID recovery", code, string(data))
	}
	request.ClientUserMessageID = "22222222222222222222222222222222"
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", request, original); code != 409 {
		t.Fatal("stale new admission", code, string(data))
	}
	if code, _ = localDeliveryHTTP(t, s, ref, "GET", "deliveries", nil, "malformed"); code == 200 {
		t.Fatal("malformed browser generation accepted")
	}
	wrong := ref
	wrong.Incarnation = strings.Repeat("7", 64)
	if code, _ = localDeliveryHTTP(t, s, wrong, "GET", "deliveries", nil, original); code == 200 {
		t.Fatal("wrong scope accepted")
	}
	if code, _ = localDeliveryHTTP(t, s, ref, "POST", "message", map[string]any{"message": "legacy", "clientUserMessageId": request.ClientUserMessageID}, original); code == 202 || f.binds != 0 {
		t.Fatal("native stale generation bypass", code, f.binds)
	}
}

func TestDurablePortalAdmissionNeverWaitsForNativeRuntimeLock(t *testing.T) {
	s, f := localDeliveryFixture(t)
	defer s.Close()
	ref := f.selected.Handle.Conversation
	lock, err := session.LockRuntimeExclusiveContext(context.Background(), s.config.AuthorityDir, ref.Session.Slug)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	inspections := len(f.inspects)
	code, data := localDeliveryHTTP(t, s, ref, "GET", "deliveries", nil)
	if code != 200 {
		t.Fatal(code, string(data))
	}
	var collection backend.DeliveryCollection
	if err = json.Unmarshal(data, &collection); err != nil || !collection.CanEnqueue {
		t.Fatal(collection, err)
	}
	request := backend.DeliveryRequest{Version: 1, ClientUserMessageID: "11111111-1111-4111-8111-111111111111", Message: "original", Delivery: "steer", Selection: collection.Selection}
	for range 2 {
		code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", request)
		if code != 202 || !strings.Contains(string(data), `"state":"queued"`) {
			t.Fatal(code, string(data))
		}
	}
	if f.binds != 0 || f.dispatches != 0 || f.lookups != 0 || len(f.inspects) != inspections {
		t.Fatal("local receipt used native work", f.binds, f.dispatches, f.lookups, f.inspects)
	}
	rows, err := s.inputDeliveryStore.List(context.Background(), collection.Scope)
	if err != nil || len(rows) != 1 || rows[0].Status.Sequence != 1 {
		t.Fatal(rows, err)
	}
	request.Message = "changed"
	code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", request)
	if code != 409 {
		t.Fatal(code, string(data))
	}
}

func TestDurablePortalExistingIDSurvivesSelectionAndCapabilityChange(t *testing.T) {
	s, f := localDeliveryFixture(t)
	defer s.Close()
	ref := f.selected.Handle.Conversation
	code, data := localDeliveryHTTP(t, s, ref, "GET", "deliveries", nil)
	if code != 200 {
		t.Fatal(code, string(data))
	}
	var metadata backend.DeliveryCollection
	json.Unmarshal(data, &metadata)
	request := backend.DeliveryRequest{Version: 1, ClientUserMessageID: "11111111111111111111111111111111", Message: "original", Delivery: "followUp", Selection: metadata.Selection}
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", request); code != 202 {
		t.Fatal(code, string(data))
	}
	original := request
	f.selected.Settings.Revision++
	f.selected.Settings.Thinking = "different"
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", request); code != 202 {
		t.Fatal("existing identity lost", code, string(data))
	}
	request.ClientUserMessageID = "22222222222222222222222222222222"
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", request); code != 409 {
		t.Fatal("stale selection admitted", code, string(data))
	}
	f.selected.CheckedInput = false
	code, data = localDeliveryHTTP(t, s, ref, "GET", "deliveries", nil)
	if code != 200 {
		t.Fatal(code, string(data))
	}
	json.Unmarshal(data, &metadata)
	if metadata.CanEnqueue || len(metadata.Entries) != 1 || metadata.Entries[0].State != "queued" {
		t.Fatal(metadata)
	}
	if code, data = localDeliveryHTTP(t, s, ref, "GET", "deliveries/11111111111111111111111111111111", nil); code != 200 {
		t.Fatal(code, string(data))
	}
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", original); code != 202 {
		t.Fatal("existing identity requires new-admission capability", code, string(data))
	}
	s.config.PiCreation.Runtime = f.piCreationRuntimeFixture // Retained worker without the optional checked-input port.
	if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", original); code != 202 {
		t.Fatal("retained worker lost original identity", code, string(data))
	}
	if f.binds != 0 || f.dispatches != 0 || f.lookups != 0 {
		t.Fatal("status read called native")
	}
}

func TestDurablePortalAdmissionChecksBoundIdentityBeforePrivateRead(t *testing.T) {
	s, f := localDeliveryFixture(t)
	defer s.Close()
	ref := f.selected.Handle.Conversation
	id := piruntime.BackendIdentity(f.selected.Handle)
	id.ScopeKey = inputdelivery.ConversationScope(ref)
	provider := piLocalDeliveries{server: s, ref: ref, identity: id}
	if err := s.inputDeliveryStore.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	store, err := inputdelivery.Open(context.Background(), s.config.UserStateRoot, s.config.Workspace, func(ctx context.Context, run func() error) error {
		calls++
		return s.inputDeliveryDatabaseGate(ctx, run)
	})
	if err != nil {
		t.Fatal(err)
	}
	s.inputDeliveryStore = store
	request := backend.DeliveryRequest{Version: 1, ClientUserMessageID: "11111111111111111111111111111111", Message: "private read guard", Delivery: "steer", Selection: strings.Repeat("a", 64)}
	for _, test := range []string{"different_identity", "noncanonical_bound_scope", "changed_profile"} {
		t.Run(test, func(t *testing.T) {
			p, supplied := provider, id
			profile := s.config.HostProfile
			defer func() { s.config.HostProfile = profile }()
			switch test {
			case "different_identity":
				supplied.ScopeKey = "different"
			case "noncanonical_bound_scope":
				p.identity.ScopeKey = "different"
				supplied = p.identity
			case "changed_profile":
				s.config.HostProfile = filepath.Join(t.TempDir(), "missing-profile")
			}
			before := calls
			status, err := p.AdmitDelivery(context.Background(), supplied, request)
			if err == nil || !reflect.DeepEqual(status, backend.DeliveryStatus{}) || calls != before {
				t.Fatal("unbound request reached private store", status, err, calls-before)
			}
		})
	}
}

func TestDurablePortalAdmissionFreshAuthorizationOverridesEveryPrivateOutcome(t *testing.T) {
	for _, outcome := range []string{"existing", "conflict", "refusal", "not_found", "store_error", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			s, f := localDeliveryFixture(t)
			defer s.Close()
			ctx := context.Background()
			ref := f.selected.Handle.Conversation
			id := piruntime.BackendIdentity(f.selected.Handle)
			id.ScopeKey = inputdelivery.ConversationScope(ref)
			provider := piLocalDeliveries{server: s, ref: ref, identity: id}
			metadata, err := provider.Deliveries(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			request := backend.DeliveryRequest{Version: 1, ClientUserMessageID: "11111111111111111111111111111111", Message: "private original", Delivery: "steer", Selection: metadata.Selection}
			switch outcome {
			case "existing", "conflict":
				if _, err = provider.AdmitDelivery(ctx, id, request); err != nil {
					t.Fatal(err)
				}
				if outcome == "conflict" {
					request.Message = "changed request"
				}
			case "refusal":
				_, err = s.inputDeliveryStore.Reject(ctx, id.ScopeKey, ref, request, "delivery_selection_changed")
				var proof *backend.DeliveryError
				if !errors.As(err, &proof) || proof.NotQueued == nil {
					t.Fatal("missing authentic refusal fixture", err)
				}
			}
			if err = s.inputDeliveryStore.Close(ctx); err != nil {
				t.Fatal(err)
			}
			// The existing transition Gate gives a deterministic point after the
			// private SELECT and before its result reaches the provider. No sleeps
			// or production hooks are needed, including for direct provider calls.
			var during func()
			var captured error
			calls := 0
			readError := errors.New("private read failed")
			s.inputDeliveryStore, err = inputdelivery.Open(ctx, s.config.UserStateRoot, s.config.Workspace, func(ctx context.Context, run func() error) error {
				err := s.inputDeliveryDatabaseGate(ctx, run)
				if during != nil {
					calls++
					if calls == 1 {
						during()
						if outcome == "store_error" {
							err = readError
						} else if outcome == "canceled" {
							err = context.Canceled
						}
						captured = err
					}
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, route := range []string{"direct", "http"} {
				for _, change := range []string{"access", "scope", "generation"} {
					t.Run(route+"/"+change, func(t *testing.T) {
						callCtx, cancel := context.WithCancel(ctx)
						defer cancel()
						var restore func()
						defer func() {
							during = nil
							if restore != nil {
								restore()
							}
						}()
						want := piruntime.ErrUnavailable
						if change == "scope" {
							want = agentbackend.ErrBindingConflict
						} else if change == "generation" {
							want = agentbackend.ErrStaleExecution
						}
						calls, captured = 0, nil
						during = func() {
							if outcome == "canceled" {
								cancel()
							}
							if change == "access" {
								team := s.installedTeams.Catalog.Teams["solo"]
								original := team.Roles["team_lead"]
								role := original
								role.Access = ""
								team.Roles["team_lead"] = role
								restore = func() { team.Roles["team_lead"] = original }
								return
							}
							path := filepath.Join(s.config.AuthorityDir, ref.Session.Slug+".json")
							field, value := "generation", strings.Repeat("9", 64)
							if change == "scope" {
								digest := sha256.Sum256([]byte(s.config.Workspace))
								path = filepath.Join(s.config.UserStateRoot, "session-backends", fmt.Sprintf("%x", digest), ref.Session.Slug+".json")
								field, value = "session_incarnation", strings.Repeat("e", 64)
							}
							original, e := os.ReadFile(path)
							if e != nil {
								t.Fatal(e)
							}
							var record map[string]any
							if e = json.Unmarshal(original, &record); e != nil {
								t.Fatal(e)
							}
							record[field] = value
							data, e := json.Marshal(record)
							if e != nil {
								t.Fatal(e)
							}
							restore = func() {
								if e := os.WriteFile(path, original, 0600); e != nil {
									t.Fatal(e)
								}
							}
							if e = os.WriteFile(path, data, 0600); e != nil {
								t.Fatal(e)
							}
						}
						catalogPath := filepath.Join(s.uploadStore.Directory, "catalog.json")
						catalog, catalogErr := os.ReadFile(catalogPath)
						if catalogErr != nil && !errors.Is(catalogErr, os.ErrNotExist) {
							t.Fatal(catalogErr)
						}
						reads, inspections := f.reads, len(f.inspects)
						if route == "direct" {
							status, e := provider.AdmitDelivery(callCtx, id, request)
							if !errors.Is(e, want) || !reflect.DeepEqual(status, backend.DeliveryStatus{}) {
								t.Fatal("authorization did not override private outcome", status, e, want)
							}
						} else {
							body, _ := json.Marshal(request)
							r := httptest.NewRequest("POST", "/codex/conversations/"+ref.Session.Slug+"/deliveries", bytes.NewReader(body)).WithContext(callCtx)
							r.Header.Set("Origin", s.config.BaseURL)
							r.Header.Set("Content-Type", "application/json")
							r.Header.Set("X-Workspace-Conversation-Scope", id.ScopeKey)
							w := httptest.NewRecorder()
							s.Handler().ServeHTTP(w, r)
							if w.Code < 400 || strings.Contains(w.Body.String(), `"notQueued"`) || strings.Contains(w.Body.String(), `"state"`) || strings.Contains(w.Body.String(), "delivery_conflict") {
								t.Fatal("private status/proof exposed", w.Code, w.Body.String())
							}
						}
						if calls != 1 || f.reads != reads || len(f.inspects) != inspections || f.binds != 0 || f.dispatches != 0 || f.lookups != 0 {
							t.Fatal("denied read reached selection, mutation or native work", calls, f.reads-reads)
						}
						after, afterErr := os.ReadFile(catalogPath)
						if !bytes.Equal(catalog, after) || (afterErr != nil && !errors.Is(afterErr, os.ErrNotExist)) || errors.Is(afterErr, os.ErrNotExist) != errors.Is(catalogErr, os.ErrNotExist) {
							t.Fatal("denied read mutated uploads", catalogErr, afterErr)
						}
						switch outcome {
						case "existing":
							if captured != nil {
								t.Fatal(captured)
							}
						case "refusal":
							var proof *backend.DeliveryError
							if !errors.As(captured, &proof) || proof.NotQueued == nil {
								t.Fatal("did not capture durable proof", captured)
							}
						default:
							expected := map[string]error{"conflict": inputdelivery.ErrConflict, "not_found": inputdelivery.ErrNotFound, "store_error": readError, "canceled": context.Canceled}[outcome]
							if !errors.Is(captured, expected) {
								t.Fatal("wrong private outcome", captured, expected)
							}
						}
					})
				}
			}
		})
	}
}

func TestDurablePortalPreflightFailureDoesNotClaim(t *testing.T) {
	for _, kind := range []string{"unavailable", "held", "stale", "canceled_after_proof"} {
		t.Run(kind, func(t *testing.T) {
			s, f := localDeliveryFixture(t)
			defer s.Close()
			parent := context.Background()
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			ref := f.selected.Handle.Conversation
			code, data := localDeliveryHTTP(t, s, ref, "GET", "deliveries", nil)
			var metadata backend.DeliveryCollection
			if code != 200 || json.Unmarshal(data, &metadata) != nil {
				t.Fatal(code, string(data))
			}
			request := backend.DeliveryRequest{Version: 1, ClientUserMessageID: strings.Repeat("a", 32), Message: "immutable original", Delivery: "steer", Selection: metadata.Selection}
			if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", request); code != 202 {
				t.Fatal(code, string(data))
			}
			row, err := s.inputDeliveryStore.Get(parent, metadata.Scope, request.ClientUserMessageID)
			if err != nil {
				t.Fatal(err)
			}
			original := inputdelivery.Digest(row.Admission)
			f.live = true
			var wanted error
			switch kind {
			case "unavailable":
				f.live = false
				wanted = piruntime.ErrUnavailable
			case "held":
				f.preflightError = piruntime.ErrHeld
				wanted = piruntime.ErrHeld
			case "stale":
				f.preflightError = piruntime.ErrScope
				wanted = piruntime.ErrScope
			case "canceled_after_proof":
				f.afterPreflight = cancel
				wanted = context.Canceled
			}
			claims := 0
			_, err = (piDeliveryDriver{s}).Dispatch(ctx, row, func(inputdelivery.ExecutionEvidence) error { claims++; return nil })
			if !errors.Is(err, wanted) || !errors.Is(err, piruntime.ErrUnavailable) || claims != 0 || f.dispatches != 0 || f.preflights != 1 || f.verifies != 0 {
				t.Fatal("preclaim failure acquired effects or lost unavailable class", kind, err, claims, f)
			}
			var terminal *inputdelivery.PreDispatchError
			if errors.As(err, &terminal) && terminal.Terminal {
				t.Fatal("temporary preflight failure became terminal", err)
			}
			saved, err := s.inputDeliveryStore.Get(parent, metadata.Scope, request.ClientUserMessageID)
			if err != nil || saved.Status.State != "queued" || saved.DispatchAtMS != 0 || inputdelivery.Digest(saved.Admission) != original {
				t.Fatal("unavailable preflight changed original claim", saved, err)
			}
		})
	}
}

func TestDurablePortalPreflightRecoveryAndClaimBoundary(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		t.Run(fmt.Sprint(lostReply), func(t *testing.T) {
			s, f := localDeliveryFixture(t)
			defer s.Close()
			ctx := context.Background()
			ref := f.selected.Handle.Conversation
			code, data := localDeliveryHTTP(t, s, ref, "GET", "deliveries", nil)
			var metadata backend.DeliveryCollection
			if code != 200 || json.Unmarshal(data, &metadata) != nil {
				t.Fatal(code, string(data))
			}
			request := backend.DeliveryRequest{Version: 1, ClientUserMessageID: strings.Repeat("b", 32), Message: "retry only the original", Delivery: "followUp", Selection: metadata.Selection}
			if code, data = localDeliveryHTTP(t, s, ref, "POST", "deliveries", request); code != 202 {
				t.Fatal(code, string(data))
			}
			get := func() inputdelivery.Record {
				r, e := s.inputDeliveryStore.Get(ctx, metadata.Scope, request.ClientUserMessageID)
				if e != nil {
					t.Fatal(e)
				}
				return r
			}
			original := get().Admission
			wait := func(ready func(inputdelivery.Record) bool) inputdelivery.Record {
				deadline := time.Now().Add(5 * time.Second)
				for {
					r := get()
					if ready(r) {
						return r
					}
					if time.Now().After(deadline) {
						t.Fatal("queue did not reach preflight boundary", r)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			// A failed live probe must remain queued with the ordinary dispatcher backoff.
			f.live = false
			d := inputdelivery.Start(ctx, s.inputDeliveryStore, piDeliveryDriver{s})
			defer d.Close()
			wait(func(r inputdelivery.Record) bool {
				return r.Status.State == "queued" && r.RetryDelayMS >= 1000 && r.NextAttemptMS > 0
			})
			d.Close()
			row := get()
			if row.DispatchAtMS != 0 || f.preflights != 1 || f.dispatches != 0 || !reflect.DeepEqual(row.Admission, original) {
				t.Fatal("unavailable original crossed claim", row, f)
			}
			f.live = true
			preflights, binds := f.preflights, f.binds
			proved := false
			f.afterPreflight = func() {
				r := get()
				if r.Status.State != "queued" || r.DispatchAtMS != 0 {
					t.Error("live proof occurred after claim", r)
				}
				proved = true
			}
			f.beforeNative = func(q piruntime.QueuedInputRequest) {
				r := get()
				if !proved || r.Status.State != "dispatching" || q.Ref != original.Ref || q.Input != original.Input || !reflect.DeepEqual(r.Admission, original) {
					t.Error("native effect lost preflight/original identity", r, q)
				}
			}
			if lostReply {
				f.nativeError = piruntime.ErrUnavailable
			}
			d = inputdelivery.Start(ctx, s.inputDeliveryStore, piDeliveryDriver{s})
			defer d.Close()
			want := "sent"
			if lostReply {
				want = "unknown"
			}
			wait(func(r inputdelivery.Record) bool { return r.Status.State == want })
			d.Close()
			if f.preflights != preflights+1 || f.binds != binds+1 || f.verifies != 0 || f.dispatches != 1 || !reflect.DeepEqual(get().Admission, original) {
				t.Fatal("recovery skipped/duplicated live proof or original", f, get())
			}
			if lostReply {
				// The claimed wire failure is original-ref Lookup-only, even after restart.
				f.nativeError = nil
				previousObservation := get().NextAttemptMS
				d = inputdelivery.Start(ctx, s.inputDeliveryStore, piDeliveryDriver{s})
				defer d.Close()
				wait(func(r inputdelivery.Record) bool {
					return r.Status.State == "unknown" && r.NextAttemptMS > previousObservation
				})
				d.Close()
				if f.dispatches != 1 || f.preflights != preflights+1 || f.lookups != 1 || !reflect.DeepEqual(get().Admission, original) {
					t.Fatal("claimed original replayed instead of lookup", f, get())
				}
			}
		})
	}
}
