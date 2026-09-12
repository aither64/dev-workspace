package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sync"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// Activity observation belongs to the portal service, independently of browser
// connections. Its client observes requests without taking approval ownership.
type activityObserver interface {
	conversation.ActivityProvider
	VerifyThread(context.Context, string, string) error
	Subscribe(context.Context, string) (<-chan struct{}, func(), error)
	Close()
}

type activityMonitor struct {
	server    *Server
	observer  activityObserver
	recorder  *codex.ActivityRecorder
	workers   map[string]activityWorker
	wg        sync.WaitGroup
	readOnce  sync.Once
	readSlots chan struct{}
	readMu    sync.Mutex
	readGates map[string]*activityReadGate
}

const activityReadConcurrency = 4

type activityReadGate struct {
	slot  chan struct{}
	users int
}

type activityWorker struct {
	threadID string
	cancel   context.CancelFunc
}

func (s *Server) startActivityMonitor() {
	if !s.config.ObserveActivity || s.config.CodexSocket == "" {
		return
	}
	authority := sha256.Sum256([]byte(s.config.CodexSocket))
	recorder, err := codex.NewActivityRecorder(filepath.Join(
		s.operationStore.directory, "activity-"+hex.EncodeToString(authority[:8]),
	))
	if err != nil {
		// Missing timing must not prevent conversation access or answering requests.
		s.config.Logger.Printf("open Codex activity recorder: %v", err)
	}
	observer := codex.NewWithOptions(s.config.CodexSocket, codex.ClientOptions{
		ClientInfo:   codex.ClientInfo{Name: "dev-workspace-observer", Title: "Workspace activity", Version: "0.1.0"},
		ObserverOnly: true, ActivityRecorder: recorder,
	})
	s.activity = &activityMonitor{server: s, observer: observer, recorder: recorder, workers: make(map[string]activityWorker)}
	s.operationWG.Add(1)
	go s.activity.run(s.operationContext)
}

func (m *activityMonitor) ReadActivity(ctx context.Context, threadID string) (codex.ActivitySnapshot, error) {
	release, err := m.acquireRead(ctx, threadID)
	if err != nil {
		return codex.ActivitySnapshot{}, err
	}
	defer release()
	return m.observer.ReadActivity(ctx, threadID)
}

// Background and browser reads share this authority's limit. Readers queue per
// thread first, so duplicate browser requests cannot consume unrelated slots.
func (m *activityMonitor) acquireRead(ctx context.Context, threadID string) (func(), error) {
	m.readOnce.Do(func() { m.readSlots = make(chan struct{}, activityReadConcurrency) })
	m.readMu.Lock()
	if m.readGates == nil {
		m.readGates = make(map[string]*activityReadGate)
	}
	gate := m.readGates[threadID]
	if gate == nil {
		gate = &activityReadGate{slot: make(chan struct{}, 1)}
		m.readGates[threadID] = gate
	}
	gate.users++
	m.readMu.Unlock()
	releaseGate := func() {
		m.readMu.Lock()
		gate.users--
		if gate.users == 0 {
			delete(m.readGates, threadID)
		}
		m.readMu.Unlock()
	}
	select {
	case gate.slot <- struct{}{}:
	case <-ctx.Done():
		releaseGate()
		return nil, ctx.Err()
	}
	select {
	case m.readSlots <- struct{}{}:
		return func() {
			<-m.readSlots
			<-gate.slot
			releaseGate()
		}, nil
	case <-ctx.Done():
		<-gate.slot
		releaseGate()
		return nil, ctx.Err()
	}
}

func (m *activityMonitor) run(ctx context.Context) {
	defer m.server.operationWG.Done()
	if m.recorder != nil {
		defer m.recorder.Close()
	}
	defer m.observer.Close()
	defer m.wg.Wait()
	defer func() {
		for _, worker := range m.workers {
			worker.cancel()
		}
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		m.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *activityMonitor) reconcile(ctx context.Context) {
	summaries, err := session.List(m.server.config.Workspace)
	if err != nil {
		m.server.config.Logger.Printf("list sessions for activity observation: %v", err)
	}
	wanted := make(map[string]string)
	for _, summary := range summaries {
		if !m.owns(summary) {
			continue
		}
		wanted[summary.Slug] = summary.Codex.ThreadID
	}
	for slug, worker := range m.workers {
		if wanted[slug] != worker.threadID {
			worker.cancel()
			delete(m.workers, slug)
		}
	}
	for slug, threadID := range wanted {
		if _, found := m.workers[slug]; found {
			continue
		}
		workerContext, cancel := context.WithCancel(ctx)
		m.workers[slug] = activityWorker{threadID: threadID, cancel: cancel}
		m.wg.Add(1)
		go m.observe(workerContext, slug, threadID)
	}
}

func (m *activityMonitor) owns(summary session.Summary) bool {
	config := m.server.config
	if summary.Archived || summary.Codex.ThreadID == "" ||
		summary.Codex.SocketPath != config.CodexSocket || summary.Creation.State != "ready" ||
		(summary.Creation.GoalSHA256 != "" && !summary.Creation.InitialGoalSent) {
		return false
	}
	authority, err := session.LoadRuntimeAuthority(config.AuthorityDir, summary.Slug, config.Workspace)
	if err != nil || authority.State != "ready" || authority.CodexThreadID != summary.Codex.ThreadID ||
		authority.CodexSocketPath != config.CodexSocket {
		return false
	}
	pending, err := session.PendingLifecycle(config.Workspace, summary.Slug)
	return err == nil && pending == ""
}

func (m *activityMonitor) observe(ctx context.Context, slug, threadID string) {
	defer m.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var events <-chan struct{}
	var unsubscribe func()
	defer func() {
		if unsubscribe != nil {
			unsubscribe()
		}
	}()
	cwd := filepath.Join(m.server.config.Workspace, "work", slug)
	nextRead := time.Time{}
	for {
		if delay := time.Until(nextRead); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		nextRead = time.Now().Add(time.Second)
		release, err := m.acquireRead(ctx, threadID)
		if err != nil {
			return
		}
		readContext, cancel := context.WithTimeout(ctx, 4*time.Second)
		err = m.observer.VerifyThread(readContext, threadID, cwd)
		if err != nil && unsubscribe != nil {
			unsubscribe()
			unsubscribe = nil
			events = nil
		}
		if err == nil && unsubscribe == nil {
			events, unsubscribe, err = m.observer.Subscribe(readContext, threadID)
		}
		interval := 5 * time.Second
		if err == nil {
			var snapshot codex.ActivitySnapshot
			snapshot, err = m.observer.ReadActivity(readContext, threadID)
			if err == nil && snapshot.CurrentState == "idle" {
				interval = 30 * time.Second
			}
		}
		ticker.Reset(interval)
		cancel()
		release()
		if ctx.Err() != nil {
			return
		}
		// Connection coverage and failed writes are represented by the recorder;
		// retrying a read never answers a pending question or approval.
		select {
		case <-ctx.Done():
			return
		case _, open := <-events:
			if !open {
				if unsubscribe != nil {
					unsubscribe()
				}
				unsubscribe = nil
				events = nil
			}
		case <-ticker.C:
		}
	}
}

func (s *Server) activityProvider() conversation.ActivityProvider {
	if s.activity != nil {
		return s.activity
	}
	if provider, ok := s.config.Codex.(conversation.ActivityProvider); ok {
		return provider
	}
	return nil
}
