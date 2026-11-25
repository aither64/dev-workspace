package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
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
	failures  map[string]activityReadFailure
	snapshots map[string]activityCachedSnapshot
	clock     func() time.Time
}

const activityReadConcurrency = 4

type activityReadGate struct {
	slot  chan struct{}
	users int
}

type activityReadFailure struct {
	count   int
	retryAt time.Time
	err     error
}

type activityCachedSnapshot struct {
	value codex.ActivitySnapshot
	at    time.Time
}

type activityWorker struct {
	threadID string
	cancel   context.CancelFunc
}

// The SDK's sinceFork segment excludes inherited turns. Always combine it
// with the immutable predecessor proof, never with a saved child aggregate.
// Current/open/latest-turn fields remain those of the raw child observation.
func composeProviderForkActivity(baseline *codex.ActivitySnapshot, raw codex.ActivitySnapshot, emptySource bool) (codex.ActivitySnapshot, error) {
	if raw.Scope != "sinceFork" || raw.ThreadID == "" {
		return codex.ActivitySnapshot{}, errors.New("current fork activity boundary is unknown")
	}
	for _, value := range []*codex.ActivitySnapshot{&raw, baseline} {
		if value == nil {
			continue
		}
		if value.Messages < 0 || value.ToolCalls < 0 || value.SentMessages < 0 || value.ReceivedMessages < 0 || value.TotalToolCalls < 0 || value.WorkingMS < 0 || value.WaitingMS < 0 || value.OpenWaitingMS < 0 || value.BetweenTurnsMS < 0 || value.UnclassifiedMS < 0 || value.IdleMS < 0 {
			return codex.ActivitySnapshot{}, errors.New("activity counters or durations are invalid")
		}
	}
	gapReason := "Timing across the provider switch is not observed."
	reason := raw.CoverageReason
	if reason == "" && baseline != nil {
		reason = baseline.CoverageReason
	}
	if !strings.Contains(reason, gapReason) {
		if reason != "" {
			reason += " "
		}
		reason += gapReason
	}
	if baseline == nil {
		if !emptySource {
			raw.CountsComplete = false
		}
		raw.CoverageComplete = false
		raw.CoverageReason = reason
		return raw, nil
	}
	if baseline.Scope != "thread" && baseline.Scope != "sinceFork" {
		return codex.ActivitySnapshot{}, errors.New("predecessor activity boundary is unknown")
	}
	result := raw
	for _, pair := range []struct {
		dst *int
		old int
	}{{&result.SentMessages, baseline.SentMessages}, {&result.ReceivedMessages, baseline.ReceivedMessages}, {&result.TotalToolCalls, baseline.TotalToolCalls}} {
		if *pair.dst < 0 || pair.old < 0 || pair.old > int(^uint(0)>>1)-*pair.dst {
			return codex.ActivitySnapshot{}, errors.New("activity count exceeds bounds")
		}
		*pair.dst += pair.old
	}
	for _, pair := range []struct {
		dst *int64
		old int64
	}{{&result.WorkingMS, baseline.WorkingMS}, {&result.WaitingMS, baseline.WaitingMS}, {&result.BetweenTurnsMS, baseline.BetweenTurnsMS}, {&result.UnclassifiedMS, baseline.UnclassifiedMS}, {&result.IdleMS, baseline.BetweenTurnsMS}} {
		if *pair.dst < 0 || pair.old < 0 || pair.old > int64(^uint64(0)>>1)-*pair.dst {
			return codex.ActivitySnapshot{}, errors.New("activity time exceeds bounds")
		}
		*pair.dst += pair.old
	}
	result.Scope = baseline.Scope
	result.CountsComplete = baseline.CountsComplete && raw.CountsComplete
	result.CoverageComplete = false // The cross-switch interval was not observed.
	result.CoverageReason = reason
	if baseline.CoverageStartMS > 0 && (result.CoverageStartMS <= 0 || baseline.CoverageStartMS < result.CoverageStartMS) {
		result.CoverageStartMS = baseline.CoverageStartMS
	}
	result.Stale = baseline.Stale || raw.Stale
	result.TimingApproximate = baseline.TimingApproximate || raw.TimingApproximate
	return result, nil
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
		HistoryReader: s.config.HistoryReader,
		ClientInfo:    codex.ClientInfo{Name: "dev-workspace-observer", Title: "Workspace activity", Version: "0.1.0"},
		ObserverOnly:  true, ActivityRecorder: recorder,
		AllowImplicitResume: func(thread string) bool {
			return s.requireMaintenanceAdmission() == nil && s.providerThreadResumeAllowed(thread) && (!s.config.RecoverSessions || s.recoveryStore().AllowsImplicitResume(s.config.CodexSocket, thread))
		},
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
	return m.readSnapshot(ctx, threadID, false)
}

// Delivery admission requires a fresh observer read. Display snapshots and
// their stale/backoff fallback cannot establish the recipient's current state.
func (m *activityMonitor) readFreshActivity(ctx context.Context, threadID string) (codex.ActivitySnapshot, error) {
	release, err := m.acquireRead(ctx, threadID)
	if err != nil {
		return codex.ActivitySnapshot{}, err
	}
	defer release()
	return m.observer.ReadActivity(ctx, threadID)
}

func (m *activityMonitor) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

func (m *activityMonitor) readSnapshot(ctx context.Context, threadID string, force bool) (codex.ActivitySnapshot, error) {
	return m.readSnapshotValidated(ctx, threadID, force, nil, nil)
}

func (m *activityMonitor) readSnapshotValidated(ctx context.Context, threadID string, force bool, validate func() error, present func(codex.ActivitySnapshot) (codex.ActivitySnapshot, error)) (codex.ActivitySnapshot, error) {
	project := func(value codex.ActivitySnapshot) (codex.ActivitySnapshot, error) {
		if present != nil {
			return present(value)
		}
		return value, nil
	}
	if validate != nil {
		if err := validate(); err != nil {
			return codex.ActivitySnapshot{}, err
		}
	}
	m.readMu.Lock()
	cached, found := m.snapshots[threadID]
	if failure, failed := m.failures[threadID]; failed && m.now().Before(failure.retryAt) {
		m.readMu.Unlock()
		if found {
			value := cached.value
			value.Stale = true
			return project(value)
		}
		return codex.ActivitySnapshot{}, failure.err
	}
	interval := 5 * time.Second
	if cached.value.CurrentState == "idle" {
		interval = 30 * time.Second
	}
	if cached.value.Refreshing {
		interval = time.Second
	}
	if found && !cached.value.Stale && !force && m.now().Sub(cached.at) < interval {
		m.readMu.Unlock()
		return project(cached.value)
	}
	m.readMu.Unlock()
	snapshot, err := m.observer.ReadActivity(ctx, threadID)
	if err == nil && validate != nil {
		err = validate()
	}
	m.readMu.Lock()
	if err == nil {
		if m.snapshots == nil {
			m.snapshots = make(map[string]activityCachedSnapshot)
		}
		m.snapshots[threadID] = activityCachedSnapshot{value: snapshot, at: m.now()}
		delete(m.failures, threadID)

	} else {
		if m.failures == nil {
			m.failures = make(map[string]activityReadFailure)
		}
		previous := m.failures[threadID]
		previous.count = min(previous.count+1, 6)
		delay := min(60*time.Second, time.Second*time.Duration(1<<previous.count))
		delay = min(60*time.Second, time.Duration(float64(delay)*(0.8+0.4*rand.Float64())))
		m.failures[threadID] = activityReadFailure{count: previous.count, retryAt: m.now().Add(delay), err: err}
		if found {
			cached.value.Stale = true
			m.snapshots[threadID] = cached
		}
	}
	m.readMu.Unlock()
	var display codex.ActivitySnapshot
	if err == nil {
		display, err = project(snapshot)
	}
	if err == nil && m.server != nil && m.server.operationStore != nil {
		if saveErr := m.server.saveThreadRecord("activity-final", threadID, display); saveErr != nil {
			m.server.config.Logger.Printf("save activity snapshot: %v", saveErr)
		}
	}
	if err != nil && found {
		value := cached.value
		value.Stale = true
		return project(value)
	}
	return display, err
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
		root, err := m.currentActivityThread(&summary, "lead", summary.Codex.ThreadID)
		if err != nil {
			continue
		}
		if !m.server.recoveryHeld(summary.Slug, root) {
			wanted[summary.Slug] = root
		}
		roster, rosterErr := m.server.loadTeamRoster(&summary)
		if rosterErr == nil && roster != nil {
			for _, member := range roster.Members {
				if member.State == "ready" && member.RemovedAt == nil && member.RetireIntent == "" && member.Thread != "" {
					current, err := m.currentActivityThread(&summary, member.Address, member.Thread)
					if err == nil && !m.server.recoveryHeld(summary.Slug, current) {
						wanted[teamConversationID(summary.Slug, member.Address)] = current
					}
				}
			}
		}
	}
	for slug, worker := range m.workers {
		if wanted[slug] != worker.threadID {
			worker.cancel()
			delete(m.workers, slug)
			m.readMu.Lock()
			delete(m.snapshots, worker.threadID)
			delete(m.failures, worker.threadID)
			m.readMu.Unlock()
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
		summary.Codex.SocketPath != config.CodexSocket ||
		(summary.HasCreation() && (summary.Creation.State != "ready" || (summary.Creation.GoalSHA256 != "" && !summary.Creation.InitialGoalSent))) ||
		(!summary.HasCreation() && session.RequireRetainedReady(&summary, config.CodexSocket) != nil) {
		return false
	}
	authority, err := session.LoadRuntimeAuthority(config.AuthorityDir, summary.Slug, config.Workspace)
	if err != nil || authority.State != "ready" || authority.CodexThreadID != summary.Codex.ThreadID ||
		authority.CodexSocketPath != config.CodexSocket {
		return false
	}
	if summary.Codex.BindingSchema == 3 {
		if _, err := m.currentActivityThread(&summary, "lead", summary.Codex.ThreadID); err != nil {
			return false
		}
	}
	pending, err := session.PendingLifecycle(config.Workspace, summary.Slug)
	return err == nil && pending == ""
}

func (m *activityMonitor) currentActivityThread(summary *session.Summary, member, original string) (string, error) {
	if summary.Codex.BindingSchema != 3 {
		return original, nil
	}
	store, err := teamruntime.NewStore(m.server.config.UserStateRoot, m.server.config.Workspace)
	if err != nil {
		return "", err
	}
	current, err := store.ResolveConversation(summary.Slug, summary.Codex.ThreadID, member, original)
	return current.ThreadID, err
}

func (m *activityMonitor) activityTarget(conversationID string) (codexproviders.ResolvedBinding, error) {
	slug, address, ok := parseTeamConversationID(conversationID)
	if !ok {
		return codexproviders.ResolvedBinding{}, errors.New("activity conversation identity changed")
	}
	summary, err := session.Find(m.server.config.Workspace, slug)
	if err != nil || summary.Archived {
		return codexproviders.ResolvedBinding{}, errors.New("activity session is unavailable")
	}
	original, member := summary.Codex.ThreadID, "lead"
	if address != "" {
		store, err := teamruntime.NewStore(m.server.config.UserStateRoot, m.server.config.Workspace)
		if err != nil {
			return codexproviders.ResolvedBinding{}, err
		}
		row, err := readyConversationMember(store, slug, summary.Codex.ThreadID, address)
		if err != nil {
			return codexproviders.ResolvedBinding{}, err
		}
		original, member = row.Thread, address
	}
	if summary.Codex.BindingSchema != 3 {
		return codexproviders.ResolvedBinding{ThreadID: original}, nil
	}
	store, err := teamruntime.NewStore(m.server.config.UserStateRoot, m.server.config.Workspace)
	if err != nil {
		return codexproviders.ResolvedBinding{}, err
	}
	return store.ResolveConversation(slug, summary.Codex.ThreadID, member, original)
}

func (m *activityMonitor) validateActivityTarget(conversationID, thread string) error {
	current, err := m.activityTarget(conversationID)
	if err != nil || current.ThreadID != thread {
		return errors.New("activity current binding changed")
	}
	return nil
}

func (m *activityMonitor) presentBoundActivity(conversationID, thread string, raw codex.ActivitySnapshot) (codex.ActivitySnapshot, error) {
	current, err := m.activityTarget(conversationID)
	if err != nil || current.ThreadID != thread || raw.ThreadID != thread {
		return codex.ActivitySnapshot{}, errors.New("activity current observation changed")
	}
	if current.Key.Incarnation == "" || current.ThreadID == current.Key.OriginalThreadID {
		return raw, nil
	}
	baseline, empty, err := providerForkActivityBaseline(m.server.config.UserStateRoot, m.server.config.Workspace, current)
	if err != nil {
		return codex.ActivitySnapshot{}, err
	}
	return composeProviderForkActivity(baseline, raw, empty)
}

type boundActivityProvider struct {
	monitor                *activityMonitor
	conversationID, thread string
}

func (p boundActivityProvider) ReadActivity(ctx context.Context, thread string) (codex.ActivitySnapshot, error) {
	if thread != p.thread {
		return codex.ActivitySnapshot{}, errors.New("activity target changed")
	}
	release, err := p.monitor.acquireRead(ctx, thread)
	if err != nil {
		return codex.ActivitySnapshot{}, err
	}
	defer release()
	return p.monitor.readSnapshotValidated(ctx, thread, false, func() error { return p.monitor.validateActivityTarget(p.conversationID, thread) }, func(raw codex.ActivitySnapshot) (codex.ActivitySnapshot, error) {
		return p.monitor.presentBoundActivity(p.conversationID, thread, raw)
	})
}

func (m *activityMonitor) lockActivityObservation(ctx context.Context, conversationID, thread string) (func(), error) {
	slug, _, ok := parseTeamConversationID(conversationID)
	if !ok {
		return nil, errors.New("activity session identity is invalid")
	}
	runtimeLock, err := session.LockRuntimeSharedContext(ctx, m.server.config.AuthorityDir, slug)
	if err != nil {
		return nil, err
	}
	store, err := teamruntime.NewStore(m.server.config.UserStateRoot, m.server.config.Workspace)
	if err != nil {
		runtimeLock.Close()
		return nil, err
	}
	unlock, err := store.LockOperation(ctx, slug)
	if err != nil {
		runtimeLock.Close()
		return nil, err
	}
	release := func() { unlock(); runtimeLock.Close() }
	if err := m.validateActivityTarget(conversationID, thread); err != nil {
		release()
		return nil, err
	}
	return release, nil
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
	sessionSlug, _, _ := parseTeamConversationID(slug)
	cwd := filepath.Join(m.server.config.Workspace, "work", sessionSlug)
	nextRead := time.Time{}
	nextActivityRead := time.Time{}
	idleWakeUsed, forceNext, lastIdle := false, false, false
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
		// Runtime -> team -> read/cache ownership matches the fork writer.
		// Release each observation cycle before waiting on subscriptions/timers.
		releaseOwner, err := m.lockActivityObservation(ctx, slug, threadID)
		if err != nil {
			return
		}
		release, err := m.acquireRead(ctx, threadID)
		if err != nil {
			releaseOwner()
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
		if err == nil && (forceNext || !m.now().Before(nextActivityRead)) {
			var snapshot codex.ActivitySnapshot
			snapshot, err = m.readSnapshotValidated(readContext, threadID, forceNext, func() error { return m.validateActivityTarget(slug, threadID) }, func(raw codex.ActivitySnapshot) (codex.ActivitySnapshot, error) {
				return m.presentBoundActivity(slug, threadID, raw)
			})
			if err == nil && snapshot.CurrentState == "idle" {
				interval = 30 * time.Second
				lastIdle = true
			} else if err == nil {
				idleWakeUsed = false
				lastIdle = false
			}
			if err == nil && snapshot.Refreshing {
				interval = time.Second
			}
			nextActivityRead = m.now().Add(interval)
		}
		forceNext = false
		nextRead = m.now().Add(time.Second)
		ticker.Reset(interval)
		cancel()
		release()
		releaseOwner()
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
			} else if lastIdle && !idleWakeUsed {
				idleWakeUsed, forceNext = true, true
				nextRead = time.Time{}
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
