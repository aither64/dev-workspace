package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/cluster"
	"github.com/aither64/dev-workspace/portal/internal/processgroup"
	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"golang.org/x/sys/unix"
)

//go:embed templates/*.html static/*
var assets embed.FS

var queueClientMessageIDPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
)
var messageDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var sessionAPIOperationPattern = regexp.MustCompile(`^[a-z]+(?:-[a-z]+)*$`)

type codexController interface {
	conversation.Client
	ReadAccountRateLimits(context.Context) (codex.AccountRateLimits, error)
	ListThreadActivity(context.Context, []workspacecodex.ThreadActivity) ([]workspacecodex.ThreadActivity, error)
	PrepareSend(string, string, string, string, bool) error
	ReconcileThreadInstructions(context.Context, string) error
}

type Config struct {
	ObserveActivity  bool
	CollectUploads   bool
	Workspace        string
	BaseURL          string
	DisplayLabel     string
	HostLabel        string
	SSHHost          string
	DevSession       string
	HostProfile      string
	GH               string
	Tmux             string
	AuthorityDir     string
	TransitionLock   string
	CodexSocket      string
	CodexVersion     string
	ClusterProviders []cluster.Provider
	UserStateRoot    string
	Logger           *log.Logger
	Codex            codexController
	VerifyThread     func(context.Context, string, string) error
	ReadThread       func(context.Context, string) (codex.Transcript, error)
}

type cachedRepositories struct {
	repositories string
	statuses     []repository.Status
	created      time.Time
	updated      time.Time
	terminal     bool
	archived     bool
}

type indexSessionStatus struct {
	Slug             string    `json:"slug"`
	UpdatedAt        time.Time `json:"updatedAt"`
	Archived         bool      `json:"archived"`
	RepositoryCount  int       `json:"repositoryCount"`
	RunningClusters  int       `json:"runningClusters"`
	PendingLifecycle string    `json:"pendingLifecycle,omitempty"`
}

type cachedIndexStatus struct {
	statuses      []indexSessionStatus
	created       time.Time
	warning       string
	authoritative bool
}

type lifecycleOperation struct {
	Slug      string                    `json:"slug,omitempty"`
	Kind      string                    `json:"kind,omitempty"`
	State     string                    `json:"state"`
	Phase     string                    `json:"phase,omitempty"`
	StartedAt string                    `json:"startedAt,omitempty"`
	UpdatedAt string                    `json:"updatedAt,omitempty"`
	Error     string                    `json:"error,omitempty"`
	Redirect  string                    `json:"redirect,omitempty"`
	ReceiptID string                    `json:"receiptId,omitempty"`
	Options   lifecycleOperationOptions `json:"options,omitempty"`
}

type lifecycleOperationOptions struct {
	Mode            string `json:"mode,omitempty"`
	AllowAbandoned  bool   `json:"allowAbandoned,omitempty"`
	Force           bool   `json:"force,omitempty"`
	TargetID        string `json:"targetId,omitempty"`
	DeletedThreadID string `json:"deletedThreadId,omitempty"`
	JournalID       string `json:"journalId,omitempty"`
	JournalExpected bool   `json:"journalExpected,omitempty"`
}

type Server struct {
	uploadCreationMu conversation.MutationLocker
	uploadStore      *uploads.Store
	uploadHandler    http.Handler
	reviewOnce       sync.Once
	reviewService    *repositoryReviewService
	activity         *activityMonitor
	config           Config
	hostProfile      hostProfileIdentity
	templates        *template.Template
	markdown         goldmark.Markdown
	sanitizer        *bluemonday.Policy
	repository       repository.Runner
	repositoryMu     sync.Mutex
	repositoryCache  map[string]cachedRepositories
	indexStatusMu    sync.Mutex
	indexStatusCache cachedIndexStatus
	indexStatusWait  chan struct{}
	codexLimitsMu    sync.Mutex
	codexLimitsCache codexLimitsSnapshot
	codexLimitsWait  *codexLimitsCall
	messageMu        sync.Mutex
	messageLocks     map[string]conversation.MutationLocker
	clusters         cluster.Runner
	operationMu      sync.Mutex
	creations        map[string]creationReceipt
	operations       map[string]lifecycleOperation
	operationStore   *lifecycleOperationStore
	operationContext context.Context
	cancelOperations context.CancelFunc
	operationWG      sync.WaitGroup
	closing          bool
	stopOnce         sync.Once
	stopping         chan struct{}
	conversation     http.Handler
}

type hostProfileIdentity struct {
	device, inode       uint64
	ctimeSec, ctimeNSec int64
	mtimeSec, mtimeNSec int64
	target              string
}

type pageData struct {
	StyleNonce        string
	BaseURL           string
	DisplayLabel      string
	HostLabel         string
	SSHHost           string
	CreationDate      string
	IndexGeneratedAt  string
	MaxMessageBytes   int
	Error             string
	Active            []session.Summary
	Archived          []session.Summary
	Session           *session.Summary
	Repositories      []repository.Status
	RepositoryWarning string
	Clusters          []cluster.Status
	Artifacts         []session.Artifact
	PendingLifecycle  string
	LifecycleOnly     bool
	LifecycleTargetID string
}

func New(config Config) (*Server, error) {
	if config.Logger == nil {
		config.Logger = log.Default()
	}
	if config.DisplayLabel == "" {
		config.DisplayLabel = "Development workspace"
	}
	if config.HostLabel == "" {
		config.HostLabel = "this host"
	}
	if config.Tmux == "" {
		config.Tmux = "tmux"
	}
	if config.VerifyThread == nil && config.Codex != nil {
		config.VerifyThread = config.Codex.VerifyThread
	}
	if config.ReadThread == nil && config.Codex != nil {
		config.ReadThread = config.Codex.ReadThread
	}
	workspace, err := filepath.Abs(config.Workspace)
	if err != nil {
		return nil, err
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	config.Workspace = workspace
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || baseURL.Path != "" || baseURL.RawQuery != "" || baseURL.Fragment != "" || baseURL.User != nil {
		return nil, fmt.Errorf("invalid base URL %q", config.BaseURL)
	}
	if baseURL.Scheme != "https" {
		return nil, fmt.Errorf("portal requires an HTTPS base URL")
	}
	if config.DevSession == "" || !filepath.IsAbs(config.DevSession) {
		return nil, errors.New("portal requires an absolute dev-session command")
	}
	if config.HostProfile == "" || !filepath.IsAbs(config.HostProfile) {
		return nil, errors.New("portal requires an absolute workspace host profile")
	}
	hostProfile, err := readHostProfileIdentity(config.HostProfile)
	if err != nil {
		return nil, fmt.Errorf("read workspace host profile: %w", err)
	}
	templates, err := template.New("pages").Funcs(template.FuncMap{
		"shortSHA": func(value string) string {
			if len(value) > 10 {
				return value[:10]
			}
			return value
		},
		"timeAgo": timeAgo,
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	policy := bluemonday.UGCPolicy()
	policy.RequireNoFollowOnLinks(true)
	policy.RequireNoReferrerOnLinks(true)
	operationContext, cancelOperations := context.WithCancel(context.Background())
	operationStore, err := newLifecycleOperationStore(workspace, config.UserStateRoot)
	if err != nil {
		cancelOperations()
		return nil, err
	}
	operations, err := operationStore.load()
	if err != nil {
		cancelOperations()
		return nil, err
	}
	server := &Server{
		uploadCreationMu: conversation.NewMutationLock(),
		config:           config, hostProfile: hostProfile, templates: templates,
		markdown: goldmark.New(goldmark.WithExtensions(extension.Table)), sanitizer: policy,
		repository:       repository.Runner{Workspace: workspace, GH: config.GH},
		clusters:         cluster.Runner{Workspace: workspace, Providers: config.ClusterProviders},
		repositoryCache:  make(map[string]cachedRepositories),
		messageLocks:     make(map[string]conversation.MutationLocker),
		operations:       operations,
		operationStore:   operationStore,
		operationContext: operationContext,
		cancelOperations: cancelOperations,
		stopping:         make(chan struct{}),
	}
	if err := server.loadCreations(); err != nil {
		cancelOperations()
		return nil, err
	}
	conversationHandler, err := conversation.NewHandler(conversation.Options{
		AllowedOrigins: []string{config.BaseURL}, BasePath: "/codex", Logger: config.Logger,
		MaxMessageBytes: session.MaxMessageBytes, Shutdown: server.stopping,
		Resolver: conversation.ResolverFunc(server.resolveConversation),
	})
	if err != nil {
		cancelOperations()
		return nil, err
	}
	server.conversation = conversationHandler
	if err := server.initUploads(); err != nil {
		cancelOperations()
		return nil, err
	}
	server.startUploadCollector()
	server.startActivityMonitor()
	if config.Codex != nil {
		server.operationWG.Add(1)
		go server.reconcileThreadInstructions()
	}
	return server, nil
}

func (s *Server) reconcileThreadInstructions() {
	defer s.operationWG.Done()
	if s.config.Codex == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.operationContext, 30*time.Second)
	defer cancel()
	summaries, err := s.listSessions()
	if err != nil {
		s.config.Logger.Printf("list sessions for Codex instruction reconciliation: %v", err)
	}
	pending := make(map[string]string)
	for _, summary := range summaries {
		if summary.Archived || summary.Codex.ThreadID == "" {
			continue
		}
		pending[summary.Slug] = summary.Codex.ThreadID
	}
	for len(pending) > 0 {
		for slug, threadID := range pending {
			if err := s.config.Codex.ReconcileThreadInstructions(ctx, threadID); err == nil {
				delete(pending, slug)
			} else if ctx.Err() != nil {
				return
			} else {
				s.config.Logger.Printf("reconcile Codex instructions for %s: %v", slug, err)
			}
		}
		if len(pending) == 0 {
			return
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Server) Close() {
	s.stopOnce.Do(func() {
		s.operationMu.Lock()
		s.closing = true
		close(s.stopping)
		s.cancelOperations()
		s.operationMu.Unlock()
		s.operationWG.Wait()
	})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	if s.conversation != nil {
		mux.Handle("/codex/", s.conversation)
	}
	if s.uploadHandler != nil {
		mux.Handle("/uploads/", s.uploadHandler)
	}
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("/", s.route)
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if looksLikeSessionAPIPath(r.URL) {
			if _, ok := sessionAPIPath(r.URL); !ok {
				http.NotFound(w, r)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
	return s.securityHeaders(guarded)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
		if !s.validMutation(r) {
			s.writeError(w, r, http.StatusForbidden, "request origin is invalid")
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		s.index(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/upload-drafts":
		s.withCreationMutation(w, r, func() { s.newUploadDraft(w, r) })
	case r.Method == http.MethodPost && r.URL.Path == "/sessions":
		s.withCreationMutation(w, r, func() { s.createSession(w, r) })
	case r.Method == http.MethodGet && r.URL.Path == "/api/models":
		s.models(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/codex-limits":
		s.codexLimits(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/collaboration-modes":
		s.collaborationModes(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/index-status":
		s.indexStatus(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/artifacts/"):
		s.artifact(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/artifact-previews/"):
		s.artifactImage(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/sessions/"):
		s.sessionAPI(w, r)
	case r.Method == http.MethodGet && strings.Count(strings.Trim(r.URL.Path, "/"), "/") == 0:
		s.sessionPage(w, r, strings.Trim(r.URL.Path, "/"))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) withPortalMutation(w http.ResponseWriter, r *http.Request, mutate func()) {
	unlock, err := s.lockTransition()
	if err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "workspace runtime is changing; retry shortly")
		return
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, err.Error())
		return
	}
	mutate()
}

func (s *Server) lockTransition() (func(), error) {
	return s.lockTransitionMode(unix.LOCK_SH)
}

func (s *Server) lockTransitionContext(ctx context.Context) (func(), error) {
	_, unlock, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	return unlock, err
}

func (s *Server) lockTransitionMode(mode int) (func(), error) {
	_, unlock, err := s.acquireTransition(mode)
	return unlock, err
}

func (s *Server) acquireTransition(mode int) (*os.File, func(), error) {
	return s.acquireTransitionContext(context.Background(), mode)
}

func (s *Server) acquireTransitionContext(
	ctx context.Context, mode int,
) (*os.File, func(), error) {
	if s.config.TransitionLock == "" {
		return nil, func() {}, nil
	}
	file, err := os.OpenFile(s.config.TransitionLock, os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			file.Close()
			return nil, nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
	return file, func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

func readHostProfileIdentity(path string) (hostProfileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return hostProfileIdentity{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFLNK {
		return hostProfileIdentity{}, errors.New("workspace host profile is not a symlink")
	}
	target, err := os.Readlink(path)
	if err != nil {
		return hostProfileIdentity{}, err
	}
	return hostProfileIdentity{
		device: uint64(stat.Dev), inode: stat.Ino,
		ctimeSec: stat.Ctim.Sec, ctimeNSec: stat.Ctim.Nsec,
		mtimeSec: stat.Mtim.Sec, mtimeNSec: stat.Mtim.Nsec,
		target: target,
	}, nil
}

func (s *Server) requireCurrentHostProfile() error {
	current, err := readHostProfileIdentity(s.config.HostProfile)
	if err != nil || current != s.hostProfile {
		return errors.New("portal belongs to a superseded workspace package generation; reload it")
	}
	return nil
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`+"\n")
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validMutation(r *http.Request) bool {
	return r.Header.Get("Origin") == s.config.BaseURL
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	summaries, err := s.listSessions()
	now := time.Now().UTC()
	data := pageData{
		BaseURL: s.config.BaseURL, CreationDate: now.Format(time.DateOnly),
		IndexGeneratedAt: now.Format(time.RFC3339Nano), MaxMessageBytes: session.MaxMessageBytes,
	}
	if err != nil {
		data.Error = err.Error()
	}
	for _, summary := range summaries {
		if summary.Archived {
			data.Archived = append(data.Archived, summary)
		} else {
			data.Active = append(data.Active, summary)
		}
	}
	s.render(w, "index", data)
}

func (s *Server) indexStatus(w http.ResponseWriter, r *http.Request) {
	result, err := s.loadIndexStatus(r.Context())
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	operations, operationErr := s.lifecycleOperations()
	if operationErr != nil {
		s.config.Logger.Printf("load lifecycle operations for index status: %v", operationErr)
		if result.warning == "" {
			result.warning = "Some lifecycle operation status is unavailable."
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"sessions": result.statuses, "generatedAt": result.created,
		"operations": operations, "warning": result.warning,
		"authoritative": result.authoritative,
	})
}

func (s *Server) loadIndexStatus(requestContext context.Context) (cachedIndexStatus, error) {
	s.indexStatusMu.Lock()
	if !s.indexStatusCache.created.IsZero() && time.Since(s.indexStatusCache.created) < 5*time.Second {
		result := cloneIndexStatus(s.indexStatusCache)
		s.indexStatusMu.Unlock()
		return result, nil
	}
	if wait := s.indexStatusWait; wait != nil {
		s.indexStatusMu.Unlock()
		select {
		case <-wait:
			s.indexStatusMu.Lock()
			result := cloneIndexStatus(s.indexStatusCache)
			s.indexStatusMu.Unlock()
			return result, nil
		case <-requestContext.Done():
			return cachedIndexStatus{}, requestContext.Err()
		}
	}
	wait := make(chan struct{})
	s.indexStatusWait = wait
	s.indexStatusMu.Unlock()

	ctx, cancel := context.WithTimeout(s.operationContext, 6*time.Second)
	result := s.computeIndexStatus(ctx)
	cancel()
	result.created = time.Now().UTC()
	s.indexStatusMu.Lock()
	s.indexStatusCache = result
	s.indexStatusWait = nil
	close(wait)
	s.indexStatusMu.Unlock()
	return cloneIndexStatus(result), nil
}

func cloneIndexStatus(source cachedIndexStatus) cachedIndexStatus {
	source.statuses = append([]indexSessionStatus(nil), source.statuses...)
	return source
}

func (s *Server) computeIndexStatus(ctx context.Context) cachedIndexStatus {
	result := cachedIndexStatus{}
	summaries, err := s.listSessions()
	result.authoritative = err == nil
	if err != nil {
		s.config.Logger.Printf("list sessions for index status: %v", err)
		result.warning = "Some session status is unavailable."
	}
	discovered, discoveryErr := s.discoverRepositories(ctx, false)
	if discoveryErr != nil {
		s.config.Logger.Printf("discover active repositories for index status: %v", discoveryErr)
		result.warning = "Some repository status is unavailable."
	}

	type clusterCount struct {
		slug  string
		count int
		err   error
	}
	clusterResults := make(chan clusterCount, len(summaries))
	var clusterWait sync.WaitGroup
	clusterLimit := make(chan struct{}, 4)
	expected := make([]workspacecodex.ThreadActivity, 0, len(summaries))
	for index := range summaries {
		summary := &summaries[index]
		if !summary.Archived {
			merged, mergeErr := session.MergeActiveRepositories(summary.Repositories, discovered[summary.Slug])
			summary.Repositories = merged
			if mergeErr != nil {
				s.config.Logger.Printf("merge repositories for %s: %v", summary.Slug, mergeErr)
			}
			if summary.Codex.ThreadID != "" {
				expected = append(expected, workspacecodex.ThreadActivity{
					ID:  summary.Codex.ThreadID,
					Cwd: filepath.Join(s.config.Workspace, "work", summary.Slug),
				})
			}
			if s.clusters.MayExist(summary.Slug) {
				clusterWait.Add(1)
				go func(slug string) {
					defer clusterWait.Done()
					select {
					case clusterLimit <- struct{}{}:
						defer func() { <-clusterLimit }()
					case <-ctx.Done():
						clusterResults <- clusterCount{slug: slug, err: ctx.Err()}
						return
					}
					clusters, inspectErr := s.clusters.InspectContext(ctx, slug)
					count := 0
					for _, item := range clusters {
						if item.State == "running" {
							count++
						}
					}
					clusterResults <- clusterCount{slug: slug, count: count, err: inspectErr}
				}(summary.Slug)
			}
		}
	}
	go func() {
		clusterWait.Wait()
		close(clusterResults)
	}()

	activityByIdentity := make(map[string]time.Time)
	if s.config.Codex != nil && len(expected) > 0 {
		activities, activityErr := s.config.Codex.ListThreadActivity(ctx, expected)
		if activityErr != nil {
			s.config.Logger.Printf("load Codex index activity: %v", activityErr)
			result.warning = "Some recent Codex activity is unavailable."
		} else {
			for _, activity := range activities {
				activityByIdentity[activity.ID+"\x00"+activity.Cwd] = activity.UpdatedAt
			}
		}
	}
	clustersBySlug := make(map[string]int)
	for item := range clusterResults {
		clustersBySlug[item.slug] = item.count
		if item.err != nil {
			s.config.Logger.Printf("inspect clusters for %s: %v", item.slug, item.err)
			result.warning = "Some development cluster status is unavailable."
		}
	}
	for _, summary := range summaries {
		updated := summary.UpdatedAt
		if !summary.Archived && summary.Codex.ThreadID != "" {
			expectedCwd := filepath.Join(s.config.Workspace, "work", summary.Slug)
			if activity := activityByIdentity[summary.Codex.ThreadID+"\x00"+expectedCwd]; activity.After(updated) {
				updated = activity
			}
		}
		pending, pendingErr := session.PendingLifecycle(s.config.Workspace, summary.Slug)
		if pendingErr != nil {
			s.config.Logger.Printf("inspect lifecycle status for %s: %v", summary.Slug, pendingErr)
			result.warning = "Some lifecycle status is unavailable."
		}
		if pending == "" {
			operation, ok, operationErr := s.lifecycleOperationForSlug(summary.Slug)
			if operationErr != nil {
				s.config.Logger.Printf("reconcile lifecycle operation for %s: %v", summary.Slug, operationErr)
				result.warning = "Some lifecycle operation status is unavailable."
			}
			if ok && (operation.State == "running" || operation.State == "failed") {
				pending = operation.Kind
			}
		}
		result.statuses = append(result.statuses, indexSessionStatus{
			Slug: summary.Slug, UpdatedAt: updated, Archived: summary.Archived,
			RepositoryCount: len(summary.Repositories),
			RunningClusters: clustersBySlug[summary.Slug], PendingLifecycle: pending,
		})
	}
	return result
}

func (s *Server) listSessions() ([]session.Summary, error) {
	summaries, listErr := session.List(s.config.Workspace)
	pending, pendingErr := session.PendingLifecycles(s.config.Workspace)
	seen := make(map[string]struct{}, len(summaries))
	for _, summary := range summaries {
		seen[summary.Slug] = struct{}{}
	}
	for slug, progress := range pending {
		if _, ok := seen[slug]; ok || progress.Operation != "delete" {
			continue
		}
		summaries = append(summaries, session.Summary{
			Manifest: session.Manifest{Slug: slug}, Lifecycle: "active",
			UpdatedAt: progress.UpdatedAt, Workspace: s.config.Workspace,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Archived != summaries[j].Archived {
			return !summaries[i].Archived
		}
		if !summaries[i].UpdatedAt.Equal(summaries[j].UpdatedAt) {
			return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
		}
		return summaries[i].Slug < summaries[j].Slug
	})
	return summaries, errors.Join(listErr, pendingErr)
}

func (s *Server) sessionPage(w http.ResponseWriter, r *http.Request, slug string) {
	if s.creationPage(w, r, slug) {
		return
	}
	summary, err := session.Find(s.config.Workspace, slug)
	if errors.Is(err, fs.ErrNotExist) {
		progress, progressErr := session.PendingLifecycleProgress(s.config.Workspace, slug)
		if progressErr != nil {
			s.writeError(w, r, http.StatusInternalServerError, progressErr.Error())
			return
		}
		if progress == nil || progress.Operation != "delete" {
			http.NotFound(w, r)
			return
		}
		summary = &session.Summary{
			Manifest: session.Manifest{Slug: slug}, Lifecycle: "active",
			UpdatedAt: progress.UpdatedAt, Workspace: s.config.Workspace,
		}
		err = nil
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if summary.Root == "" {
		data := pageData{
			BaseURL: s.config.BaseURL, Session: summary, PendingLifecycle: "delete",
			CreationDate: time.Now().Format(time.DateOnly), MaxMessageBytes: session.MaxMessageBytes,
			LifecycleOnly: true,
		}
		s.render(w, "session", data)
		return
	}
	lifecycleTargetID, lifecycleIdentityErr := lifecycleTargetIdentity(summary)
	s.normalizeInteractivity(r.Context(), summary)
	var discoveryErr error
	if !summary.Archived {
		var repositories []session.Repository
		repositories, discoveryErr = s.activeRepositories(r.Context(), summary)
		summary.Repositories = repositories
		if discoveryErr != nil {
			s.config.Logger.Printf("discover repositories for %s: %v", summary.Slug, discoveryErr)
		}
	}
	data := pageData{
		BaseURL: s.config.BaseURL, Session: summary,
		CreationDate: time.Now().Format(time.DateOnly), MaxMessageBytes: session.MaxMessageBytes,
		LifecycleTargetID: lifecycleTargetID,
	}
	if lifecycleIdentityErr != nil {
		data.Error = "Session lifecycle identity is unavailable: " + lifecycleIdentityErr.Error()
	}
	data.PendingLifecycle, err = session.PendingLifecycle(s.config.Workspace, summary.Slug)
	if err != nil {
		data.Error = "Session lifecycle state is unsafe: " + err.Error()
	}
	if discoveryErr != nil {
		if data.Error != "" {
			data.Error += "; "
		}
		data.Error += "Some live worktrees could not be verified: " + discoveryErr.Error()
	}
	data.Repositories = s.repository.Skeleton(summary.Slug, summary.Repositories, summary.Archived)
	data.Clusters, err = s.clusters.InspectContext(r.Context(), summary.Slug)
	if err != nil {
		if data.Error != "" {
			data.Error += "; "
		}
		data.Error += "Some development cluster details are unavailable: " + err.Error()
	}
	data.Artifacts = session.AvailableArtifacts(summary)
	if receipt, ok := s.currentCreation(slug); ok && receipt.State == "conflict" {
		if data.Error != "" {
			data.Error += " "
		}
		data.Error += receipt.Phase
	}
	s.render(w, "session", data)
}

// sessionDetails refreshes the existing sections without replacing the conversation.
func (s *Server) sessionDetails(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	var warning string
	if !summary.Archived {
		repositories, err := s.activeRepositories(r.Context(), summary)
		if err != nil {
			s.config.Logger.Printf("refresh repositories for %s: %v", summary.Slug, err)
			warning = "Some live worktrees could not be verified: " + err.Error()
		}
		summary.Repositories = repositories
	}
	data := pageData{Session: summary, Repositories: s.repositories(r.Context(), summary), RepositoryWarning: warning, Artifacts: session.AvailableArtifacts(summary)}
	var repositories, artifacts bytes.Buffer
	if err := s.templates.ExecuteTemplate(&repositories, "repositories", data); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Unable to render repositories"})
		return
	}
	if err := s.templates.ExecuteTemplate(&artifacts, "artifact-list", data); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Unable to render artifacts"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"repositoriesHTML": repositories.String(), "artifactsHTML": artifacts.String(),
		"repositoryCount": len(data.Repositories), "artifactCount": len(data.Artifacts),
	})
}

func (s *Server) repositories(ctx context.Context, summary *session.Summary) []repository.Status {
	repositoryJSON, _ := json.Marshal(summary.Repositories)
	cacheDuration := 5 * time.Second
	if summary.Archived {
		cacheDuration = time.Minute
	}
	s.repositoryMu.Lock()
	if cached, ok := s.repositoryCache[summary.Slug]; ok && time.Since(cached.created) < cacheDuration && cached.repositories == string(repositoryJSON) &&
		cached.updated.Equal(summary.ManifestUpdatedAt) && cached.terminal == summary.Terminal && cached.archived == summary.Archived {
		result := append([]repository.Status(nil), cached.statuses...)
		s.repositoryMu.Unlock()
		return result
	}
	s.repositoryMu.Unlock()
	inspectionContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	statuses := s.repository.Inspect(
		inspectionContext, summary.Slug, summary.Repositories, summary.Archived,
	)
	s.repositoryMu.Lock()
	s.repositoryCache[summary.Slug] = cachedRepositories{
		statuses: append([]repository.Status(nil), statuses...), created: time.Now(), repositories: string(repositoryJSON),
		updated: summary.ManifestUpdatedAt, terminal: summary.Terminal, archived: summary.Archived,
	}
	s.repositoryMu.Unlock()
	return statuses
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, session.MaxFormRequestBodyBytes)
	if err := r.ParseForm(); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "invalid form")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	goal := strings.TrimSpace(r.FormValue("goal"))
	model := strings.TrimSpace(r.FormValue("model"))
	effort := strings.TrimSpace(r.FormValue("effort"))
	creationDate := strings.TrimSpace(r.FormValue("creation_date"))
	if !session.ValidSlug(name) || len(name) > 48 {
		s.writeError(w, r, http.StatusBadRequest, "session name is invalid")
		return
	}
	if (goal == "" && len(r.Form["attachmentIds"]) == 0) || len([]byte(goal)) > session.MaxMessageBytes {
		s.writeError(w, r, http.StatusBadRequest, fmt.Sprintf(
			"initial request is required and must be at most %s bytes",
			session.FormattedMaxMessageBytes(),
		))
		return
	}
	if parsed, err := time.Parse(time.DateOnly, creationDate); err != nil || parsed.Format(time.DateOnly) != creationDate {
		s.writeError(w, r, http.StatusBadRequest, "session creation date is invalid")
		return
	}
	if err := validateCreationSettings(model, effort); err != nil {
		s.writeError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.uploadCreationMu.Lock(r.Context()); err != nil {
		s.writeError(w, r, 409, "Session creation is busy; retry shortly")
		return
	}
	defer s.uploadCreationMu.Unlock()
	if err := s.retainCreationUploads(r.Context()); err != nil {
		s.writeError(w, r, 500, "Unable to reconcile initial uploads")
		return
	}
	preparedGoal, prepareErr := s.prepareCreationAttachments(r.Context(), creationDate+"-"+name, r.FormValue("uploadScope"), goal, r.Form["attachmentIds"])
	if prepareErr != nil {
		s.writeError(w, r, http.StatusBadRequest, prepareErr.Error())
		return
	}
	goal = preparedGoal
	receipt, err := s.acceptCreation(creationRequest{
		Kind: "new", Slug: creationDate + "-" + name, Goal: goal, Model: model, Effort: effort,
	})
	if err != nil {
		s.writeError(w, r, http.StatusConflict, err.Error())
		return
	}
	if err := s.uploadStore.RetainCreations(r.Context(), map[string]uploads.Scope{goal: {Slug: receipt.Request.Slug, Epoch: receipt.DeletionHistorySHA256}}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "Session creation was accepted; reload to check its progress")
		return
	}
	http.Redirect(w, r, receipt.status().URL, http.StatusSeeOther)
}

func (s *Server) runDevSession(parent context.Context, timeout time.Duration, args ...string) (string, string, error) {
	commandCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	command := exec.Command(s.config.DevSession, args...)
	return runDevSessionCommand(commandCtx, command)
}

func (s *Server) runDevSessionWithTransition(
	parent context.Context, timeout time.Duration, transition *os.File, args ...string,
) (string, string, error) {
	commandCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	command := exec.Command(s.config.DevSession, args...)
	if transition != nil {
		command.ExtraFiles = []*os.File{transition}
		command.Env = append(os.Environ(), "DEV_WORKSPACE_TRANSITION_LOCK_FD=3")
	}
	return runDevSessionCommand(commandCtx, command)
}

func runDevSessionCommand(commandCtx context.Context, command *exec.Cmd) (string, string, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := processgroup.Run(commandCtx, command)
	return stdout.String(), stderr.String(), err
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if s.config.Codex == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Codex model catalog is unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	models, err := s.config.Codex.ListModels(ctx)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if models == nil {
		models = []codex.Model{}
	}
	for index := range models {
		models[index].IsDefault = models[index].Model == workspacecodex.DefaultNewThreadModel
	}
	s.writeJSON(w, http.StatusOK, models)
}

func (s *Server) collaborationModes(w http.ResponseWriter, r *http.Request) {
	if s.config.Codex == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "Codex collaboration modes are unavailable",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	modes, err := s.config.Codex.ListCollaborationModes(ctx)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if modes == nil {
		modes = []codex.CollaborationMode{}
	}
	s.writeJSON(w, http.StatusOK, modes)
}

func (s *Server) validateCollaborationMode(ctx context.Context, requested string) error {
	if requested == "" {
		return nil
	}
	if s.config.Codex == nil {
		return errors.New("Codex collaboration modes are unavailable")
	}
	lookupContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	modes, err := s.config.Codex.ListCollaborationModes(lookupContext)
	if err != nil {
		return fmt.Errorf("load Codex collaboration modes: %w", err)
	}
	for _, mode := range modes {
		if mode.Mode == requested {
			return nil
		}
	}
	return fmt.Errorf("Codex collaboration mode %q is not available", requested)
}

func (s *Server) validateModelSettings(ctx context.Context, settings codex.ThreadSettings, allowDefault bool) error {
	if settings.Model == "" {
		if settings.ReasoningEffort != "" || !allowDefault {
			return errors.New("select a Codex model before choosing a reasoning effort")
		}
		return nil
	}
	if s.config.Codex == nil {
		return errors.New("Codex model catalog is unavailable")
	}
	lookupContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	models, err := s.config.Codex.ListModels(lookupContext)
	if err != nil {
		return fmt.Errorf("load Codex models: %w", err)
	}
	for _, model := range models {
		if model.Model != settings.Model {
			continue
		}
		if settings.ReasoningEffort == "" {
			return nil
		}
		for _, effort := range model.SupportedReasoningEfforts {
			if effort.ReasoningEffort == settings.ReasoningEffort {
				return nil
			}
		}
		return fmt.Errorf("reasoning effort %q is not available for %s", settings.ReasoningEffort, model.DisplayName)
	}
	return fmt.Errorf("Codex model %q is not available", settings.Model)
}

func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	remainder := strings.TrimPrefix(r.URL.Path, "/artifacts/")
	s.serveArtifactFile(w, r, remainder, false)
}

func (s *Server) artifactImage(w http.ResponseWriter, r *http.Request) {
	remainder := strings.TrimPrefix(r.URL.Path, "/artifact-previews/")
	s.serveArtifactFile(w, r, remainder, true)
}

func artifactContentType(path string) (string, string, bool) {
	extension := strings.ToLower(filepath.Ext(path))
	types := map[string]struct {
		kind        string
		contentType string
	}{
		".gif": {"image", "image/gif"}, ".jpeg": {"image", "image/jpeg"},
		".jpg": {"image", "image/jpeg"}, ".png": {"image", "image/png"},
		".webp": {"image", "image/webp"}, ".json": {"text", "application/json"},
		".log":  {"text", "text/plain; charset=utf-8"},
		".md":   {"markdown", "text/markdown; charset=utf-8"},
		".txt":  {"text", "text/plain; charset=utf-8"},
		".yaml": {"text", "application/yaml"}, ".yml": {"text", "application/yaml"},
	}
	presentation, ok := types[extension]
	return presentation.kind, presentation.contentType, ok
}

func (s *Server) serveArtifactFile(w http.ResponseWriter, r *http.Request, remainder string, imageOnly bool) {
	parts := strings.SplitN(remainder, "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	summary, err := session.Find(s.config.Workspace, parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file, info, err := session.OpenArtifact(summary, parts[1], 10*1024*1024)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	kind, contentType, allowed := artifactContentType(parts[1])
	if !allowed || (imageOnly && kind != "image") {
		http.Error(w, "artifact type is not available", http.StatusUnsupportedMediaType)
		return
	}
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Content-Type", contentType)
	disposition := "attachment"
	if imageOnly {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disposition, info.Name()))
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (s *Server) artifactPreview(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	kind, _, allowed := artifactContentType(path)
	if !allowed {
		s.writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "artifact type is not available"})
		return
	}
	file, _, err := session.OpenArtifact(summary, path, 10*1024*1024)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "artifact is not available"})
		return
	}
	defer file.Close()
	if kind == "image" {
		s.writeJSON(w, http.StatusOK, map[string]string{
			"kind": "image", "url": "/artifact-previews/" +
				url.PathEscape(summary.Slug) + "/" + strings.ReplaceAll(url.PathEscape(path), "%2F", "/"),
		})
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, 10*1024*1024+1))
	if err != nil || len(data) > 10*1024*1024 {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unable to read artifact"})
		return
	}
	text := strings.ToValidUTF8(string(data), "�")
	if kind == "markdown" {
		s.writeJSON(w, http.StatusOK, map[string]string{
			"kind": "markdown", "html": string(s.renderTextMarkdown(text)),
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"kind": "text", "text": text})
}

func (s *Server) sessionAPI(w http.ResponseWriter, r *http.Request) {
	parts, ok := sessionAPIPath(r.URL)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if s.creationAPI(w, r, parts) || s.earlyCreationRequest(w, r, parts) {
		return
	}
	if conversationPath := legacyConversationPath(parts, r.Method); conversationPath != "" && s.conversation != nil {
		decodedPath, err := url.PathUnescape(conversationPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		request := r.Clone(r.Context())
		requestURL := *r.URL
		requestURL.Path = decodedPath
		requestURL.RawPath = conversationPath
		request.URL = &requestURL
		s.conversation.ServeHTTP(w, request)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodGet && parts[1] == "operation" {
		s.lifecycleStatus(w, parts[0])
		return
	}
	if len(parts) == 2 && r.Method == http.MethodDelete && parts[1] == "operation" {
		s.dismissLifecycleOperation(w, r, parts[0])
		return
	}
	if len(parts) == 3 && r.Method == http.MethodPost &&
		parts[1] == "operation" && parts[2] == "retry" {
		s.retryLifecycleOperation(w, r, parts[0])
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost && parts[1] == "delete" {
		s.deleteSession(w, r, parts[0])
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost && parts[1] == "archive" {
		summary, err := session.Find(s.config.Workspace, parts[0])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		s.startArchive(w, r, summary)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost && parts[1] == "revive" {
		summary, err := session.Find(s.config.Workspace, parts[0])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		s.startRevive(w, r, summary)
		return
	}
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
		s.withPortalMutation(w, r, func() { s.sessionAPIResolved(w, r, parts) })
		return
	}
	s.sessionAPIResolved(w, r, parts)
}

func looksLikeSessionAPIPath(requestURL *url.URL) bool {
	escaped := requestURL.EscapedPath()
	return strings.HasPrefix(escaped, "/api/sessions/") ||
		strings.HasPrefix(requestURL.Path, "/api/sessions/") ||
		strings.HasPrefix(path.Clean(escaped), "/api/sessions/") ||
		strings.HasPrefix(path.Clean(requestURL.Path), "/api/sessions/")
}

func sessionAPIPath(requestURL *url.URL) ([]string, bool) {
	escaped := requestURL.EscapedPath()
	if requestURL.RawPath != "" && requestURL.RawPath != escaped {
		return nil, false
	}
	remainder, found := strings.CutPrefix(escaped, "/api/sessions/")
	if !found {
		return nil, false
	}
	parts := strings.Split(remainder, "/")
	if len(parts) < 2 || len(parts) > 3 || !session.ValidSlug(parts[0]) ||
		!sessionAPIOperationPattern.MatchString(parts[1]) {
		return nil, false
	}
	for _, part := range parts {
		if part == "" {
			return nil, false
		}
	}
	if len(parts) == 3 && parts[1] != "queue" &&
		!((parts[1] == "operation" || parts[1] == "creation") && parts[2] == "retry") {
		return nil, false
	}
	return parts, true
}

func legacyConversationPath(parts []string, method string) string {
	if len(parts) == 2 {
		allowed := map[string]map[string]bool{
			http.MethodGet: {
				"activity": true, "events": true, "pending": true, "queue": true, "thread": true,
			},
			http.MethodPost: {
				"interrupt": true, "message": true, "message-ack": true,
				"queue": true, "respond": true, "settings": true,
			},
		}
		if allowed[method][parts[1]] {
			return "/codex/conversations/" + strings.Join(parts, "/")
		}
	}
	if len(parts) == 3 && parts[1] == "queue" &&
		(method == http.MethodDelete ||
			(method == http.MethodPost && (parts[2] == "start" || parts[2] == "reconcile"))) {
		return "/codex/conversations/" + strings.Join(parts, "/")
	}
	return ""
}

func (s *Server) sessionAPIResolved(w http.ResponseWriter, r *http.Request, parts []string) {
	summary, err := session.Find(s.config.Workspace, parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.sessionAPIForSummary(w, r, parts, summary)
}

func (s *Server) sessionAPIForSummary(
	w http.ResponseWriter, r *http.Request, parts []string, summary *session.Summary,
) {
	if s.repositoryReviewAPI(w, r, summary, parts) {
		return
	}
	if len(parts) == 2 && r.Method == http.MethodGet && parts[1] == "details" {
		s.sessionDetails(w, r, summary)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost && parts[1] == "release-cluster" {
		s.releaseCluster(w, r, summary)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodGet && parts[1] == "artifact-preview" {
		s.artifactPreview(w, r, summary)
		return
	}
	s.normalizeInteractivity(r.Context(), summary)
	if summary.Codex.ThreadID == "" {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "session has no Codex thread"})
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost && parts[1] == "fork" {
		if !summary.Interactive {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "session is not ready to fork"})
			return
		}
		s.forkSession(w, r, summary)
		return
	}
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
		lock, err := session.LockRuntimeShared(s.config.AuthorityDir, summary.Slug)
		if err != nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		defer lock.Close()
		owner, journalErr := session.PendingLifecycle(s.config.Workspace, summary.Slug)
		if journalErr != nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": journalErr.Error()})
			return
		}
		if owner != "" {
			s.writeJSON(w, http.StatusConflict, map[string]string{
				"error": "session " + owner + " is unfinished; retry that operation first",
			})
			return
		}
		summary, err = session.Find(s.config.Workspace, parts[0])
		if err != nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "session state changed"})
			return
		}
		s.normalizeInteractivity(r.Context(), summary)
		if !summary.Interactive {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "session is not ready for browser changes"})
			return
		}
	}
	if len(parts) == 2 && r.Method == http.MethodPost && parts[1] == "implement-plan" {
		s.implementPlan(w, r, summary)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request, slug string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Force    bool   `json:"force"`
		TargetID string `json:"targetId"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	owner, journalErr := session.PendingLifecycle(s.config.Workspace, slug)
	if journalErr != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": journalErr.Error()})
		return
	}
	if owner != "" && owner != "delete" {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "session " + owner + " is unfinished; retry that operation first",
		})
		return
	}
	if owner == "delete" {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Deletion is already in progress. Use the retry action.",
		})
		return
	}
	force := body.Force
	deletionTargetID, deletedThreadID, targetErr := s.resolveDeletionTarget(
		slug, false, body.TargetID, "",
	)
	if targetErr != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": targetErr.Error()})
		return
	}
	deletionJournalID, targetErr := newLifecycleOperationID()
	if targetErr != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "Unable to create a deletion operation identity.",
		})
		return
	}
	args := []string{"delete", slug, "--as-is", "--portal-authorized"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--portal-operation-id", deletionJournalID)
	s.startLifecycleOperation(w, slug, "delete", "/", args, lifecycleOperationOptions{
		Force: force, TargetID: deletionTargetID, DeletedThreadID: deletedThreadID,
		JournalID: deletionJournalID,
	}, "")
}

func newLifecycleOperationID() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return hex.EncodeToString(random), nil
}

func lifecycleTargetIdentity(summary *session.Summary) (string, error) {
	if summary == nil || summary.Root == "" {
		return "", errors.New("session tracking is missing")
	}
	tracking := filepath.Join(summary.Workspace, summary.Root, summary.Slug)
	var stat unix.Stat_t
	if err := unix.Lstat(tracking, &stat); err != nil {
		return "", fmt.Errorf("inspect session tracking: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", errors.New("session tracking is not a directory")
	}
	identity := fmt.Sprintf(
		"%s\x00%d\x00%d\x00%d\x00%d\x00%s", filepath.Clean(tracking), stat.Dev, stat.Ino,
		stat.Ctim.Sec, stat.Ctim.Nsec, summary.Codex.ThreadID,
	)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity))), nil
}

func validateLifecycleTarget(
	summary *session.Summary, expectedTargetID, action string,
) (string, error) {
	currentTargetID, err := lifecycleTargetIdentity(summary)
	if err != nil {
		return "", fmt.Errorf("verify %s target: %w", action, err)
	}
	if expectedTargetID == "" || expectedTargetID != currentTargetID {
		return "", fmt.Errorf(
			"This %s request does not match the current session. Reload the page and confirm it again.",
			action,
		)
	}
	return currentTargetID, nil
}

func (s *Server) revalidateLifecycleTarget(slug, kind, expectedTargetID string) error {
	summary, err := session.Find(s.config.Workspace, slug)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("The session no longer exists. Reload the workspace before retrying it.")
	}
	if err != nil {
		return fmt.Errorf("verify %s target: %w", kind, err)
	}
	_, err = validateLifecycleTarget(summary, expectedTargetID, kind)
	return err
}

func (s *Server) resolveDeletionTarget(
	slug string, journalOwned bool, expectedTargetID, retainedThreadID string,
) (string, string, error) {
	if journalOwned {
		return expectedTargetID, retainedThreadID, nil
	}
	summary, err := session.Find(s.config.Workspace, slug)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", errors.New("The session no longer exists. Reload the workspace before deleting it.")
	}
	if err != nil {
		return "", "", fmt.Errorf("verify deletion target: %w", err)
	}
	currentTargetID, err := validateLifecycleTarget(summary, expectedTargetID, "delete")
	if err != nil {
		return "", "", err
	}
	currentThreadID := summary.Codex.ThreadID
	return currentTargetID, currentThreadID, nil
}

func completedPlan(transcript codex.Transcript) (codex.TranscriptEntry, bool) {
	for index := len(transcript.Entries) - 1; index >= 0; index-- {
		entry := transcript.Entries[index]
		if entry.Kind == "plan" && entry.TurnStatus == "completed" && strings.TrimSpace(entry.Text) != "" {
			return entry, true
		}
	}
	return codex.TranscriptEntry{}, false
}

func planDigest(text string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
}

func (s *Server) implementPlan(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	r.Body = http.MaxBytesReader(w, r.Body, session.MaxFormRequestBodyBytes)
	var body struct {
		PlanText            string `json:"planText"`
		Model               string `json:"model"`
		ReasoningEffort     string `json:"reasoningEffort"`
		Action              string `json:"action"`
		PlanTurnID          string `json:"planTurnId"`
		PlanSHA256          string `json:"planSha256"`
		ClientUserMessageID string `json:"clientUserMessageId"`
		Name                string `json:"name"`
		CreationDate        string `json:"creationDate"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Action) == "new" {
		destination, err := creationDestination(strings.TrimSpace(body.Name), strings.TrimSpace(body.CreationDate))
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.acceptPlanCreation(w, r, summary, creationRequest{
			Kind: "plan", Slug: destination, PlanTurnID: strings.TrimSpace(body.PlanTurnID),
			PlanSHA256: strings.TrimSpace(body.PlanSHA256), PlanText: body.PlanText,
			Model: strings.TrimSpace(body.Model), Effort: strings.TrimSpace(body.ReasoningEffort),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	messageLock := s.messageLock(summary.Slug)
	if err := messageLock.Lock(ctx); err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	defer messageLock.Unlock()
	transcript, err := s.config.Codex.ReadThread(ctx, summary.Codex.ThreadID)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	plan, ok := completedPlan(transcript)
	digest := planDigest(plan.Text)
	if !ok || plan.TurnID != strings.TrimSpace(body.PlanTurnID) ||
		digest != strings.TrimSpace(body.PlanSHA256) {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "the displayed plan is stale; review the latest plan before implementing it",
		})
		return
	}
	if transcript.Status == "active" {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Codex is still working on this plan"})
		return
	}
	if transcript.CollaborationMode != "plan" && strings.TrimSpace(body.Action) != "same" {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "the conversation is no longer in Plan mode",
		})
		return
	}

	switch strings.TrimSpace(body.Action) {
	case "same":
		const implementationMessage = "Implement the plan."
		body.ClientUserMessageID = strings.TrimSpace(body.ClientUserMessageID)
		if !queueClientMessageIDPattern.MatchString(body.ClientUserMessageID) {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message has an invalid client identity"})
			return
		}
		actionContext := "plan:" + digest
		if transcript.CollaborationMode == "plan" {
			if err := s.config.Codex.PrepareSend(
				summary.Codex.ThreadID, implementationMessage,
				body.ClientUserMessageID, actionContext, false,
			); err != nil {
				s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
				return
			}
			mode := "default"
			if _, err := s.config.Codex.UpdateThreadSettings(
				ctx, summary.Codex.ThreadID,
				codex.ThreadSettingsUpdate{CollaborationMode: &mode},
			); err != nil {
				s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
		} else if transcript.CollaborationMode == "default" {
			attempted, attemptErr := s.config.Codex.SendAttempted(
				ctx, summary.Codex.ThreadID, implementationMessage,
				body.ClientUserMessageID, actionContext,
			)
			if attemptErr != nil {
				s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": attemptErr.Error()})
				return
			}
			if !attempted {
				s.writeJSON(w, http.StatusConflict, map[string]string{
					"error": "the conversation is no longer in Plan mode",
				})
				return
			}
		} else {
			s.writeJSON(w, http.StatusConflict, map[string]string{
				"error": "the conversation is no longer in Plan mode",
			})
			return
		}
		receipt, err := s.config.Codex.Send(
			ctx, summary.Codex.ThreadID, implementationMessage,
			body.ClientUserMessageID, actionContext,
		)
		if err != nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusAccepted, receipt)
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "select how to implement the plan"})
	}
}

func (s *Server) releaseCluster(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	if summary.Archived {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "archived sessions cannot have live development clusters"})
		return
	}
	lock, lockErr := session.LockRuntimeShared(s.config.AuthorityDir, summary.Slug)
	if lockErr != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": lockErr.Error()})
		return
	}
	defer lock.Close()
	owner, journalErr := session.PendingLifecycle(s.config.Workspace, summary.Slug)
	if journalErr != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": journalErr.Error()})
		return
	}
	if owner != "" {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "session " + owner + " is unfinished; retry that operation first",
		})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Kind string `json:"kind"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	if err := s.clusters.Release(ctx, strings.TrimSpace(body.Kind), summary.Slug); err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) startArchive(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Mode     string `json:"mode"`
		TargetID string `json:"targetId"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	owner, journalErr := session.PendingLifecycle(s.config.Workspace, summary.Slug)
	if journalErr != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": journalErr.Error()})
		return
	}
	if owner != "" {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Session " + owner + " is already in progress. Use the retry action.",
		})
		return
	}
	if summary.Archived {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "session is already archived"})
		return
	}
	if body.Mode != "complete" && body.Mode != "abandoned" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "select completed or abandoned archival"})
		return
	}
	targetID, err := validateLifecycleTarget(summary, body.TargetID, "archive")
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	args := []string{"archive", summary.Slug, "--as-is", "--portal-authorized"}
	if body.Mode == "abandoned" {
		args = append(args, "--abandoned")
	}
	journalID, err := newLifecycleOperationID()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "Unable to create an archive operation identity.",
		})
		return
	}
	args = append(args, "--portal-operation-id", journalID)
	s.startLifecycleOperation(
		w, summary.Slug, "archive", "/", args,
		lifecycleOperationOptions{
			Mode: body.Mode, TargetID: targetID, JournalID: journalID,
		}, "",
	)
}

func (s *Server) startRevive(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		AllowAbandoned bool   `json:"allowAbandoned"`
		TargetID       string `json:"targetId"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	owner, journalErr := session.PendingLifecycle(s.config.Workspace, summary.Slug)
	if journalErr != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": journalErr.Error()})
		return
	}
	if owner != "" {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Session " + owner + " is already in progress. Use the retry action.",
		})
		return
	}
	if !summary.Archived {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "session is already active"})
		return
	}
	if summary.Lifecycle == "abandoned" && !body.AllowAbandoned {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "confirm that the abandoned session should be revived"})
		return
	}
	targetID, err := validateLifecycleTarget(summary, body.TargetID, "revive")
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	args := []string{"revive", summary.Slug, "--as-is", "--portal-authorized"}
	if summary.Lifecycle == "abandoned" {
		args = append(args, "--allow-abandoned")
	}
	journalID, err := newLifecycleOperationID()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "Unable to create a revive operation identity.",
		})
		return
	}
	args = append(args, "--portal-operation-id", journalID)
	s.startLifecycleOperation(
		w, summary.Slug, "revive", "/"+summary.Slug+"/", args,
		lifecycleOperationOptions{
			AllowAbandoned: body.AllowAbandoned || summary.Lifecycle == "abandoned",
			TargetID:       targetID,
			JournalID:      journalID,
		}, "",
	)
}

func (s *Server) startLifecycleOperation(
	w http.ResponseWriter, slug, kind, redirect string, args []string,
	options lifecycleOperationOptions, expectedReceiptID string,
) {
	owner, err := session.PendingLifecycle(s.config.Workspace, slug)
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if owner != "" && owner != kind {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "session " + owner + " is unfinished; retry that operation first",
		})
		return
	}
	s.operationMu.Lock()
	if s.closing {
		s.operationMu.Unlock()
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "portal is shutting down"})
		return
	}
	operation, exists := s.operations[slug]
	if expectedReceiptID != "" &&
		(!exists || operation.ReceiptID != expectedReceiptID ||
			(operation.State != "failed" && operation.State != "paused")) {
		s.operationMu.Unlock()
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "The lifecycle operation changed. Reload the page before retrying it.",
		})
		return
	}
	if exists && operation.State == "running" {
		s.operationMu.Unlock()
		if operation.Kind != kind {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "another session operation is already running"})
			return
		}
		s.writeJSON(w, http.StatusAccepted, operation)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	startedAt := now
	if expectedReceiptID != "" {
		startedAt = operation.StartedAt
	}
	receiptID, err := newLifecycleOperationID()
	if err != nil {
		s.operationMu.Unlock()
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "unable to create the lifecycle operation",
		})
		return
	}
	operation = lifecycleOperation{
		Slug: slug, Kind: kind, State: "running", Phase: "starting", Redirect: redirect,
		StartedAt: startedAt, UpdatedAt: now, ReceiptID: receiptID, Options: options,
	}
	if err := s.operationStore.canPersistTerminalOutcomes(s.operations, slug, operation); err != nil {
		s.operationMu.Unlock()
		s.writeJSON(w, http.StatusInsufficientStorage, map[string]string{
			"error": "lifecycle operation history is full; dismiss finished operations and retry",
		})
		return
	}
	if err := s.replaceLifecycleOperationLocked(slug, operation); err != nil {
		s.operationMu.Unlock()
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "unable to persist the lifecycle operation",
		})
		return
	}
	s.operationWG.Add(1)
	s.operationMu.Unlock()
	go func(started lifecycleOperation) {
		defer s.operationWG.Done()
		err := s.runLifecycleOperation(s.operationContext, slug, kind, args, started.Options)
		finished := time.Now().UTC().Format(time.RFC3339Nano)
		completed := started
		completed.UpdatedAt = finished
		if err == nil {
			completed.State = "complete"
			completed.Phase = "complete"
		} else {
			completed.State = "failed"
			completed.Error = boundedLifecycleError(err.Error())
			if progress, progressErr := session.PendingLifecycleProgress(s.config.Workspace, slug); progressErr == nil && progress != nil && progress.Operation == kind {
				completed.Phase = progress.Phase
				if completed.Options.JournalID != "" &&
					completed.Options.JournalID == progress.JournalID {
					completed.applyProgressOptions(*progress)
				}
			}
		}
		s.operationMu.Lock()
		current, ownsReceipt := s.operations[slug]
		if !ownsReceipt || current.ReceiptID == "" ||
			current.ReceiptID != completed.ReceiptID {
			s.config.Logger.Printf(
				"discard stale %s operation result for %s", kind, slug,
			)
			s.operationMu.Unlock()
			return
		}
		if persistErr := s.replaceLifecycleOperationLocked(slug, completed); persistErr != nil {
			s.config.Logger.Printf("persist completed %s operation for %s: %v", kind, slug, persistErr)
			completed.State = "failed"
			completed.Error = "The command ended, but the portal could not save its final status. Check the session state and retry."
			s.operations[slug] = completed
		}
		s.operationMu.Unlock()
	}(operation)
	s.writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) runLifecycleOperation(
	parent context.Context, slug, kind string, args []string, options lifecycleOperationOptions,
) error {
	return s.executeLifecycleOperation(parent, 12*time.Minute, slug, kind, args, options)
}

func (s *Server) executeLifecycleOperation(
	parent context.Context, timeout time.Duration, slug, kind string, args []string,
	options lifecycleOperationOptions,
) error {
	transition, unlockTransition, err := s.acquireTransition(unix.LOCK_EX)
	if err != nil {
		return fmt.Errorf("lock workspace runtime for %s: %w", kind, err)
	}
	defer unlockTransition()
	if err := s.requireCurrentHostProfile(); err != nil {
		return err
	}
	mutationLock := s.messageLock(slug)
	if err := mutationLock.Lock(parent); err != nil {
		return fmt.Errorf("lock conversation mutations for %s: %w", kind, err)
	}
	defer mutationLock.Unlock()
	progress, progressErr := session.PendingLifecycleProgress(s.config.Workspace, slug)
	if progressErr != nil {
		return fmt.Errorf("verify %s operation identity: %w", kind, progressErr)
	}
	if options.JournalExpected {
		if progress == nil || progress.Operation != kind ||
			progress.JournalID == "" || progress.JournalID != options.JournalID {
			return errors.New("The lifecycle operation changed. Reload the page before retrying it.")
		}
	} else if progress != nil {
		return errors.New("Another lifecycle operation started. Reload the page before retrying it.")
	}
	if kind == "delete" {
		if _, _, targetErr := s.resolveDeletionTarget(
			slug, progress != nil, options.TargetID, options.DeletedThreadID,
		); targetErr != nil {
			return targetErr
		}
	} else if progress == nil {
		if targetErr := s.revalidateLifecycleTarget(slug, kind, options.TargetID); targetErr != nil {
			return targetErr
		}
	}
	stdout, stderr, err := s.runDevSessionWithTransition(parent, timeout, transition, args...)
	if err != nil {
		return commandFailure(kind+" session", stdout, stderr, err)
	}
	s.repositoryMu.Lock()
	delete(s.repositoryCache, slug)
	s.repositoryMu.Unlock()
	return nil
}

func (s *Server) lifecycleStatus(w http.ResponseWriter, slug string) {
	operation, ok, err := s.lifecycleOperationForSlug(slug)
	if err != nil {
		if !ok {
			operation = lifecycleOperation{
				Slug: slug, State: "failed",
				Error:     boundedLifecycleError("Session lifecycle state is unsafe: " + err.Error()),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			}
		}
	} else if !ok {
		operation = lifecycleOperation{State: "idle"}
	}
	s.writeJSON(w, http.StatusOK, operation)
}

func (s *Server) retryLifecycleOperation(w http.ResponseWriter, r *http.Request, slug string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		JournalID string `json:"journalId"`
		ReceiptID string `json:"receiptId"`
		Force     *bool  `json:"force"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	operation, ok, err := s.lifecycleOperationForSlug(slug)
	if err != nil && !ok {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "lifecycle operation is not available"})
		return
	}
	if operation.State != "failed" && operation.State != "paused" {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "lifecycle operation is not retryable"})
		return
	}
	if body.ReceiptID == "" || operation.ReceiptID == "" || body.ReceiptID != operation.ReceiptID {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "The lifecycle operation changed. Reload the page before retrying it.",
		})
		return
	}
	owner, ownerErr := session.PendingLifecycle(s.config.Workspace, slug)
	if ownerErr != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": ownerErr.Error()})
		return
	}
	if owner != "" && owner != operation.Kind {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "session " + owner + " is unfinished; retry that operation first",
		})
		return
	}
	progress, progressErr := session.PendingLifecycleProgress(s.config.Workspace, slug)
	if progressErr != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": progressErr.Error()})
		return
	}
	if body.JournalID == "" || body.JournalID != operation.Options.JournalID {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "The lifecycle operation changed. Reload the page before retrying it.",
		})
		return
	}
	if body.Force != nil && operation.Kind != "delete" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Force is available only when retrying deletion.",
		})
		return
	}
	if progress != nil {
		if progress.Operation != operation.Kind || body.JournalID != progress.JournalID {
			s.writeJSON(w, http.StatusConflict, map[string]string{
				"error": "The lifecycle operation changed. Reload the page before retrying it.",
			})
			return
		}
		operation.applyProgressOptions(*progress)
	} else if operation.Options.JournalExpected {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "The lifecycle recovery journal is no longer available. Reload the workspace before retrying.",
		})
		return
	}
	if progress == nil && operation.Kind != "delete" {
		if targetErr := s.revalidateLifecycleTarget(
			slug, operation.Kind, operation.Options.TargetID,
		); targetErr != nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": targetErr.Error()})
			return
		}
	}
	if operation.Kind == "delete" && body.Force != nil && *body.Force {
		operation.Options.Force = true
	}

	args := []string{operation.Kind, slug, "--as-is", "--portal-authorized"}
	switch operation.Kind {
	case "archive":
		if operation.Options.Mode != "complete" && operation.Options.Mode != "abandoned" {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "archive retry mode is unavailable"})
			return
		}
		if operation.Options.Mode == "abandoned" {
			args = append(args, "--abandoned")
		}
	case "delete":
		deletionTargetID, deletedThreadID, targetErr := s.resolveDeletionTarget(
			slug, owner == "delete", operation.Options.TargetID,
			operation.Options.DeletedThreadID,
		)
		if targetErr != nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": targetErr.Error()})
			return
		}
		operation.Options.TargetID = deletionTargetID
		operation.Options.DeletedThreadID = deletedThreadID
		if operation.Options.Force {
			args = append(args, "--force")
		}
	case "revive":
		if operation.Options.AllowAbandoned {
			args = append(args, "--allow-abandoned")
		}
	default:
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "lifecycle operation is invalid"})
		return
	}
	args = append(args, "--portal-operation-id", operation.Options.JournalID)
	s.startLifecycleOperation(
		w, slug, operation.Kind, operation.Redirect, args, operation.Options,
		operation.ReceiptID,
	)
}

func (s *Server) dismissLifecycleOperation(w http.ResponseWriter, r *http.Request, slug string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		ReceiptID string `json:"receiptId"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	operation, ok, err := s.lifecycleOperationForSlug(slug)
	if err != nil && !ok {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if operation.State != "failed" && operation.State != "complete" {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "running or paused lifecycle operations cannot be dismissed",
		})
		return
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	current, exists := s.operations[slug]
	if !exists || body.ReceiptID == "" || current.ReceiptID == "" ||
		body.ReceiptID != current.ReceiptID {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "The lifecycle operation changed. Reload the page before dismissing it.",
		})
		return
	}
	if current.State != "failed" && current.State != "complete" {
		s.writeJSON(w, http.StatusConflict, map[string]string{
			"error": "running or paused lifecycle operations cannot be dismissed",
		})
		return
	}
	if err := s.removeLifecycleOperationLocked(slug); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "unable to dismiss the lifecycle operation",
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func commandFailure(action, stdout, stderr string, err error) error {
	message := strings.TrimSpace(stderr)
	if message == "" {
		message = strings.TrimSpace(stdout)
	}
	if message == "" {
		message = err.Error()
	}
	lines := strings.Split(strings.ToValidUTF8(message, "�"), "\n")
	concise := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if marker := strings.Index(line, "error: command failed:"); marker >= 0 {
			line = strings.TrimSpace(line[:marker])
		}
		if line == "" || strings.HasPrefix(line, "command failed:") ||
			(strings.HasPrefix(line, "error: command failed") && strings.Contains(line, "/nix/store/")) {
			continue
		}
		concise = append(concise, line)
	}
	if len(concise) == 0 {
		concise = append(concise, err.Error())
	}
	return errors.New(boundedLifecycleError(action + ": " + strings.Join(concise, "\n")))
}

func (s *Server) forkSession(w http.ResponseWriter, r *http.Request, source *session.Summary) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var body struct {
		Name            string `json:"name"`
		CreationDate    string `json:"creationDate"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoningEffort"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.CreationDate = strings.TrimSpace(body.CreationDate)
	body.Model = strings.TrimSpace(body.Model)
	body.ReasoningEffort = strings.TrimSpace(body.ReasoningEffort)
	if !session.ValidSlug(body.Name) || len(body.Name) > 48 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session name is invalid"})
		return
	}
	if parsed, err := time.Parse(time.DateOnly, body.CreationDate); err != nil ||
		parsed.Format(time.DateOnly) != body.CreationDate {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session creation date is invalid"})
		return
	}
	if err := validateCreationSettings(body.Model, body.ReasoningEffort); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	current, identity, err := s.creationSource(source.Slug)
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	receipt, err := s.acceptCreation(creationRequest{
		Kind: "fork", Slug: body.CreationDate + "-" + body.Name,
		Source: current.Slug, SourceThreadID: current.Codex.ThreadID, SourceIdentity: identity,
		Model: body.Model, Effort: body.ReasoningEffort,
	})
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusAccepted, receipt.status())
}

func (s *Server) normalizeInteractivity(parent context.Context, summary *session.Summary) {
	persisted := summary.Codex
	summary.Interactive = false
	summary.Codex = session.Codex{}
	summary.Tmux = session.Tmux{}
	if s.config.VerifyThread == nil || s.config.CodexSocket == "" || s.config.CodexVersion == "" ||
		persisted.ThreadID == "" || persisted.SocketPath != s.config.CodexSocket {
		return
	}
	expectedCwd := filepath.Join(s.config.Workspace, "work", summary.Slug)
	verifyContext, verifyCancel := context.WithTimeout(parent, 3*time.Second)
	defer verifyCancel()
	if err := s.config.VerifyThread(verifyContext, persisted.ThreadID, expectedCwd); err != nil {
		return
	}
	// A persisted thread with configured endpoint provenance and App Server cwd is
	// safe for passive transcript reads. Live host authority is still required
	// for every control and mutation.
	summary.Codex = persisted
	creationReady := summary.Creation.State == "ready" &&
		(summary.Creation.GoalSHA256 == "" || summary.Creation.InitialGoalSent)
	if summary.Archived || !creationReady || s.config.AuthorityDir == "" {
		return
	}
	authority, err := session.LoadRuntimeAuthority(
		s.config.AuthorityDir, summary.Slug, s.config.Workspace,
	)
	if err != nil || authority.State != "ready" ||
		authority.CodexThreadID != persisted.ThreadID ||
		authority.CodexSocketPath != s.config.CodexSocket {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	if err := authority.VerifyTmux(ctx, s.config.Tmux); err != nil {
		return
	}
	if err := s.config.VerifyThread(
		ctx, authority.CodexThreadID, expectedCwd,
	); err != nil {
		return
	}
	summary.Tmux = session.Tmux{
		SocketPath: authority.TmuxSocket, CodexThreadID: authority.CodexThreadID,
		CodexSocketPath:    authority.CodexSocketPath,
		CodexClientVersion: authority.CodexClientVersion,
	}
	summary.Interactive = true
}

func (s *Server) resolveConversation(
	ctx context.Context, request conversation.ResolveRequest,
) (conversation.Target, error) {
	if !session.ValidSlug(request.ID) {
		return conversation.Target{}, errors.New("invalid session identity")
	}
	var releases []func()
	release := func() {
		for index := len(releases) - 1; index >= 0; index-- {
			releases[index]()
		}
	}
	failed := true
	defer func() {
		if failed {
			release()
		}
	}()
	if request.Mutation {
		if receipt, ok := s.currentCreation(request.ID); ok && receipt.blocksSession() {
			return conversation.Target{}, errors.New("session initialization has not finished")
		}
	}
	if request.Mutation {
		unlock, err := s.lockTransitionContext(ctx)
		if err != nil {
			return conversation.Target{}, fmt.Errorf("lock workspace transition: %w", err)
		}
		releases = append(releases, unlock)
		if err := s.requireCurrentHostProfile(); err != nil {
			return conversation.Target{}, err
		}
	}
	summary, err := session.Find(s.config.Workspace, request.ID)
	if err != nil {
		return conversation.Target{}, err
	}
	s.normalizeInteractivity(ctx, summary)
	if summary.Codex.ThreadID == "" {
		return conversation.Target{}, errors.New("session has no verified Codex thread")
	}
	if request.Mutation {
		runtimeLock, err := session.LockRuntimeShared(s.config.AuthorityDir, summary.Slug)
		if err != nil {
			return conversation.Target{}, fmt.Errorf("lock session runtime: %w", err)
		}
		releases = append(releases, func() { _ = runtimeLock.Close() })
		owner, err := session.PendingLifecycle(s.config.Workspace, summary.Slug)
		if err != nil {
			return conversation.Target{}, err
		}
		if owner != "" {
			return conversation.Target{}, fmt.Errorf("session %s is unfinished", owner)
		}
		summary, err = session.Find(s.config.Workspace, request.ID)
		if err != nil {
			return conversation.Target{}, errors.New("session state changed")
		}
		s.normalizeInteractivity(ctx, summary)
	}
	interactive := summary.Interactive
	if request.Mutation && !interactive {
		return conversation.Target{}, errors.New("session is not ready for browser changes")
	}
	if request.Operation == "events" && !interactive {
		return conversation.Target{}, errors.New("session is not interactive")
	}
	capabilities := conversation.Capabilities{Read: true}
	if interactive {
		capabilities = conversation.Capabilities{
			Read: true, Pending: true, QueueRead: true,
			Send: true, Queue: true, Interrupt: true,
			Settings: true, Respond: true, EventStream: true,
		}
	}
	expectedCwd := filepath.Join(s.config.Workspace, "work", summary.Slug)
	var attachments conversation.AttachmentProvider
	if s.uploadStore != nil {
		backend, err := s.sessionUploads(ctx, summary.Slug, summary.Codex.ThreadID, !interactive)
		if err != nil {
			return conversation.Target{}, err
		}
		attachments = backend
	}
	var once sync.Once
	failed = false
	return conversation.Target{
		Attachments: attachments,
		Client:      s.config.Codex, ThreadID: summary.Codex.ThreadID, Directory: expectedCwd,
		Capabilities: capabilities, MutationLock: s.messageLock(summary.Slug),
		TransformTranscript: s.presentTranscript,
		Activity:            s.activityProvider(),
		Release:             func() { once.Do(release) },
	}, nil
}

func (s *Server) presentTranscript(transcript *codex.Transcript) {
	for index := range transcript.Entries {
		entry := &transcript.Entries[index]
		if entry.Text != "" &&
			(entry.Kind == "agentMessage" || entry.Kind == "reasoning" || entry.Kind == "plan") {
			entry.HTML = string(s.renderTextMarkdown(entry.Text))
		}
	}
}

func (s *Server) messageLock(threadID string) conversation.MutationLocker {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	lock := s.messageLocks[threadID]
	if lock == nil {
		lock = conversation.NewMutationLock()
		s.messageLocks[threadID] = lock
	}
	return lock
}

func (s *Server) renderTextMarkdown(text string) template.HTML {
	var output strings.Builder
	if err := s.markdown.Convert([]byte(text), &output); err != nil {
		return template.HTML("<p class=\"notice error\">Unable to render document.</p>")
	}
	return template.HTML(s.sanitizer.Sanitize(output.String())) // #nosec G203 -- sanitized by bluemonday.
}

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	s.renderStatus(w, http.StatusOK, name, data)
}
func (s *Server) renderStatus(w http.ResponseWriter, status int, name string, data pageData) {
	if name == "session" {
		var nonce [24]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			http.Error(w, "Unable to render page", http.StatusInternalServerError)
			return
		}
		data.StyleNonce = hex.EncodeToString(nonce[:])
		policy := w.Header().Get("Content-Security-Policy")
		w.Header().Set("Content-Security-Policy", strings.Replace(policy,
			"style-src 'self'", "style-src 'self' 'nonce-"+data.StyleNonce+"'", 1))
	}
	data.DisplayLabel = s.config.DisplayLabel
	data.HostLabel = s.config.HostLabel
	data.SSHHost = s.config.SSHHost
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		s.config.Logger.Printf("render %s: %v", name, err)
	}
}
func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.writeJSON(w, status, map[string]string{"error": message})
		return
	}
	http.Error(w, message, status)
}
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, session.MaxJSONRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request must contain one JSON value"})
		return false
	}
	return true
}

func timeAgo(value time.Time) string {
	duration := time.Since(value)
	if duration < time.Minute {
		return "just now"
	}
	if duration < time.Hour {
		return fmt.Sprintf("%dm ago", int(duration.Minutes()))
	}
	if duration < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(duration.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(duration.Hours()/24))
}
