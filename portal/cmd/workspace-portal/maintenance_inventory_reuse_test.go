package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
)

type maintenanceUnknownDelivery struct {
	monitoroperation.DeliveryTransport
}

func (maintenanceUnknownDelivery) Ready(context.Context, monitoroperation.Owner) (string, error) {
	return "", nil
}
func (maintenanceUnknownDelivery) Prepare(context.Context, monitoroperation.Owner) (codex.TurnOptions, error) {
	return codex.TurnOptions{}, nil
}
func (maintenanceUnknownDelivery) Send(context.Context, monitoroperation.Owner, monitoroperation.Event) (codex.SendReceipt, error) {
	return codex.SendReceipt{}, &codex.UnknownSendOutcomeError{Err: errors.New("disposable unknown receipt")}
}

func maintenanceReuseObserver(t *testing.T, service *monitoroperation.Service, owner monitoroperation.Owner, id string) monitoroperation.ObserverRecord {
	t.Helper()
	record, err := service.StartCIObservation(context.Background(), owner.RootThreadID, monitoroperation.StartRequest{ID: id, Brief: "Disposable late observer", Source: monitoroperation.Source{Kind: "ci", Provider: "github", Repository: "example/project", WorkflowID: "12", RunID: "34", RunAttempt: 1, Commit: strings.Repeat("a", 40)}})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestMaintenanceInventoryReuseLateObserverFreshness(t *testing.T) {
	for _, mode := range []string{"poll", "receipt", "new", "corrupt", "collision"} {
		t.Run(mode, func(t *testing.T) {
			m, o, service, native, op, summary := maintenanceMonitorFixture(t)
			completeMaintenanceMonitorProof(t, service, native, op)
			var record monitoroperation.ObserverRecord
			if mode != "new" && mode != "receipt" {
				record = maintenanceReuseObserver(t, service, op.Owner, strings.Repeat("3", 32))
			}
			if mode == "receipt" {
				// The existing joined terminal fixture supplies an event that can
				// enter submitting through the ordinary delivery transaction.
				completeMaintenanceObserverProof(t, service, op.Owner)
				var err error
				record, err = service.Store.GetObserver(context.Background(), strings.Repeat("3", 32))
				if err != nil {
					t.Fatal(err)
				}
			}
			entered, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			var proofErr error
			native.reads = nil
			native.readHook = func(ctx context.Context, stage, _ string) error {
				if stage == "native-idle" {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
				return ctx.Err()
			}
			mutations := native.mutations
			go func() {
				defer close(joined)
				_, proofErr = maintenanceMonitorInventory(context.Background(), service.Store, summary, o.Services[0].Socket, native)
			}()
			defer func() { once.Do(func() { close(release) }); <-joined }()
			select {
			case <-entered:
			case <-joined:
				t.Fatal("proof stopped before controlled native boundary", proofErr)
			}
			path := filepath.Join(m.store.Root, "monitor-operations")
			// Locate only the selected disposable observer's owning file.
			var observerPath string
			if mode != "new" {
				if err := filepath.WalkDir(path, func(path string, entry os.DirEntry, err error) error {
					if err == nil && !entry.IsDir() && entry.Name() == record.ID+".json" && filepath.Base(filepath.Dir(path)) == "observers" {
						observerPath = path
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if observerPath == "" {
					t.Fatal("missing disposable observer")
				}
			}
			switch mode {
			case "poll", "new":
				if mode == "new" {
					record = maintenanceReuseObserver(t, service, op.Owner, strings.Repeat("4", 32))
				}
				service.ObserverEpoch = "disposable-worker"
				if _, err := service.ClaimCIPoll(context.Background(), record.ID); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				delivery := monitoroperation.DeliveryService{Store: service.Store, Resolve: service.Resolve, LockSession: service.LockSession, Transport: maintenanceUnknownDelivery{}}
				if err := delivery.Dispatch(context.Background(), record.ID, record.Events[0].ID); err != nil {
					t.Fatal(err)
				}
				current, err := service.Store.GetObserver(context.Background(), record.ID)
				if err != nil || current.Events[0].Delivery.Status != "submitting" {
					t.Fatal("receipt fixture did not enter submitting", err)
				}
			case "corrupt":
				if err := os.WriteFile(observerPath, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "collision":
				record.ID = op.ID
				data, _ := json.Marshal(record)
				if _, err := monitoroperation.DecodeObserverRecord(data); err != nil {
					t.Fatal("collision control is not independently valid", err)
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(observerPath), op.ID+".json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			once.Do(func() { close(release) })
			<-joined
			if proofErr == nil || native.mutations != mutations {
				t.Fatal("late change was reused away or changed native effects", proofErr)
			}
			var detail *monitoroperation.DiagnosticError
			if !errors.As(proofErr, &detail) {
				t.Fatal("typed refusal lost", proofErr)
			}
			if (mode == "poll" || mode == "receipt" || mode == "new") && (detail.Stage != "receipt-proof" || detail.MonitorID != record.ID) {
				t.Fatal("late blocker identity lost", detail)
			}
			t.Logf("%s: late fresh observer scan refused at %s; original native proof joined", mode, detail.Stage)
		})
	}
}

func TestMaintenanceInventoryReuseNextCallFreshness(t *testing.T) {
	for _, change := range []string{"receipt", "root", "new"} {
		t.Run(change, func(t *testing.T) {
			m, o, service, native, op, summary := maintenanceMonitorFixture(t)
			completeMaintenanceMonitorProof(t, service, native, op)
			if _, err := maintenanceMonitorInventory(context.Background(), service.Store, summary, o.Services[0].Socket, native); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "receipt":
				record := maintenanceReuseObserver(t, service, op.Owner, strings.Repeat("3", 32))
				service.ObserverEpoch = "worker"
				if _, err := service.ClaimCIPoll(context.Background(), record.ID); err != nil {
					t.Fatal(err)
				}
			case "root":
				path := filepath.Join(m.workspace, "work", summary.Slug, "portal.yml")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.Replace(string(data), "thread_id: thread", "thread_id: other-root", 1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "new":
				native.operation = strings.Repeat("4", 32)
				if _, err := service.Start(context.Background(), op.Owner.RootThreadID, monitoroperation.StartRequest{ID: native.operation, Brief: "New disposable operation", Source: monitoroperation.Source{Kind: "local", Argv: []string{"fixture"}, Cwd: m.workspace, InputIdentity: "fixture", Revision: "fixture", InvocationID: native.operation}}); err != nil {
					t.Fatal(err)
				}
			}
			before, mutations := maintenanceMonitorProofBytes(t, m.store.Root), native.mutations
			native.reads = nil
			if _, err := maintenanceMonitorInventory(context.Background(), service.Store, summary, o.Services[0].Socket, native); err == nil {
				t.Fatal("next call reused stale admission", change)
			}
			if native.mutations != mutations || !reflect.DeepEqual(before, maintenanceMonitorProofBytes(t, m.store.Root)) {
				t.Fatal("next call mutated records or native state")
			}
		})
	}
}

func TestMaintenanceInventoryReuseForeignValidationBeforeNative(t *testing.T) {
	for _, format := range []string{"native", "observer"} {
		for _, fault := range []string{"malformed", "duplicate", "workspace", "filename", "schema", "mode", "oversize", "collision"} {
			t.Run(format+"/"+fault, func(t *testing.T) {
				m, o, service, native, target, summary := maintenanceMonitorFixture(t)
				completeMaintenanceMonitorProof(t, service, native, target)
				foreign := *service
				owner := target.Owner
				owner.Slug = "unrelated-owner"
				foreign.ResolveLead = func(context.Context, string) (monitoroperation.Owner, error) { return owner, nil }
				foreign.Resolve = func(string) (monitoroperation.Owner, error) { return owner, nil }
				id := strings.Repeat("4", 32)
				var record any
				if format == "observer" {
					record = maintenanceReuseObserver(t, &foreign, owner, id)
				} else {
					native.operation = id
					cwd := native.cwd
					native.cwd = filepath.Join(m.workspace, "work", owner.Slug)
					op, err := foreign.Start(context.Background(), owner.RootThreadID, monitoroperation.StartRequest{ID: id, Brief: "Foreign disposable validation", Source: monitoroperation.Source{Kind: "local", Argv: []string{"fixture"}, Cwd: m.workspace, InputIdentity: "fixture", Revision: "fixture", InvocationID: id}})
					native.cwd = cwd
					if err != nil {
						t.Fatal(err)
					}
					record = op
				}
				var path string
				if err := filepath.WalkDir(filepath.Join(m.store.Root, "monitor-operations"), func(candidate string, entry os.DirEntry, err error) error {
					if err == nil && !entry.IsDir() && entry.Name() == id+".json" && (filepath.Base(filepath.Dir(candidate)) == "observers") == (format == "observer") {
						path = candidate
					}
					return err
				}); err != nil || path == "" {
					t.Fatal("foreign fixture path unavailable", err)
				}
				valid, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data := valid
				switch fault {
				case "malformed":
					data = []byte("{")
				case "duplicate":
					data = []byte(strings.Replace(string(valid), `"schema":1`, `"schema":1,"schema":1`, 1))
				case "workspace":
					data = []byte(strings.Replace(string(valid), `"workspace":"`+m.workspace+`"`, `"workspace":"`+m.workspace+`/different"`, 1))
				case "schema":
					data = []byte(strings.Replace(string(valid), `"schema":1`, `"schema":99`, 1))
				case "oversize":
					data = bytes.Repeat([]byte(" "), 512*1024+1)
				case "filename":
					if err := os.Rename(path, filepath.Join(filepath.Dir(path), strings.Repeat("5", 32)+".json")); err != nil {
						t.Fatal(err)
					}
				case "mode":
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				case "collision":
					// Both formats are valid independently; the shared ID is not.
					if format == "native" {
						observer := maintenanceReuseObserver(t, &foreign, owner, strings.Repeat("6", 32))
						observer.ID = id
						data, _ = json.Marshal(observer)
						if _, err := monitoroperation.DecodeObserverRecord(data); err != nil {
							t.Fatal(err)
						}
						path = filepath.Join(filepath.Dir(path), "observers", id+".json")
					} else {
						observer := record.(monitoroperation.ObserverRecord)
						observer.ID = target.ID
						data, _ = json.Marshal(observer)
						if _, err := monitoroperation.DecodeObserverRecord(data); err != nil {
							t.Fatal(err)
						}
						path = filepath.Join(filepath.Dir(path), target.ID+".json")
					}
				}
				if fault != "mode" && fault != "filename" {
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				native.reads = nil
				before, mutations := maintenanceMonitorProofBytes(t, m.store.Root), native.mutations
				if _, err := maintenanceMonitorInventory(context.Background(), service.Store, summary, o.Services[0].Socket, native); err == nil {
					t.Fatal("foreign corruption bypassed strict validation", fault)
				}
				if len(native.reads) != 0 || native.mutations != mutations || !reflect.DeepEqual(before, maintenanceMonitorProofBytes(t, m.store.Root)) {
					t.Fatal("foreign validation ran native proof or changed state")
				}
			})
		}
	}
}
