package web

import (
	"bytes"
	"context"
	"embed"
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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aither64/vpsfree-cz-workspace/portal/internal/codex"
	"github.com/aither64/vpsfree-cz-workspace/portal/internal/repository"
	"github.com/aither64/vpsfree-cz-workspace/portal/internal/session"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
)

//go:embed templates/*.html static/*
var assets embed.FS

type codexController interface {
	VerifyThread(context.Context, string, string) error
	ReadThread(context.Context, string) (codex.Transcript, error)
	Send(context.Context, string, string) error
	Interrupt(context.Context, string) error
	Subscribe(context.Context, string) (<-chan struct{}, func(), error)
	PromptsWithItems(context.Context, string) ([]codex.Prompt, error)
	RespondAnswers(context.Context, string, string, map[string]map[string][]string) error
	RespondDecision(context.Context, string, string, string) error
}

type Config struct {
	Workspace     string
	BaseURL       string
	DevSession    string
	GH            string
	Tmux          string
	TmuxSocket    string
	AuthorityDir  string
	CodexSocket   string
	CodexVersion  string
	CodexCommand  string
	PortalCommand string
	Logger        *log.Logger
	Codex         codexController
	VerifyThread  func(context.Context, string, string) error
	ReadThread    func(context.Context, string) (codex.Transcript, error)
}

type cachedRepositories struct {
	statuses []repository.Status
	created  time.Time
	updated  time.Time
	closed   bool
	archived bool
}

type Server struct {
	config          Config
	templates       *template.Template
	markdown        goldmark.Markdown
	sanitizer       *bluemonday.Policy
	repository      repository.Runner
	repositoryMu    sync.Mutex
	repositoryCache map[string]cachedRepositories
	messageMu       sync.Mutex
	messageLocks    map[string]*sync.Mutex
	stopOnce        sync.Once
	stopping        chan struct{}
}

type pageData struct {
	BaseURL         string
	CreationDate    string
	MaxMessageBytes int
	Error           string
	Active          []session.Summary
	Archived        []session.Summary
	Session         *session.Summary
	Repositories    []repository.Status
	Plan            template.HTML
	State           template.HTML
}

func New(config Config) (*Server, error) {
	if config.Logger == nil {
		config.Logger = log.Default()
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
	return &Server{
		config: config, templates: templates,
		markdown: goldmark.New(), sanitizer: policy,
		repository:      repository.Runner{GH: config.GH},
		repositoryCache: make(map[string]cachedRepositories),
		messageLocks:    make(map[string]*sync.Mutex),
		stopping:        make(chan struct{}),
	}, nil
}

func (s *Server) Close() {
	s.stopOnce.Do(func() { close(s.stopping) })
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("/", s.route)
	return s.securityHeaders(mux)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if !s.validMutation(r) {
			s.writeError(w, r, http.StatusForbidden, "request origin is invalid")
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		s.index(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/sessions":
		s.createSession(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/artifacts/"):
		s.artifact(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/sessions/"):
		s.sessionAPI(w, r)
	case r.Method == http.MethodGet && strings.Count(strings.Trim(r.URL.Path, "/"), "/") == 0:
		s.sessionPage(w, r, strings.Trim(r.URL.Path, "/"))
	default:
		http.NotFound(w, r)
	}
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

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	summaries, err := session.List(s.config.Workspace)
	data := pageData{
		BaseURL: s.config.BaseURL, CreationDate: time.Now().Format(time.DateOnly),
		MaxMessageBytes: session.MaxMessageBytes,
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

func (s *Server) sessionPage(w http.ResponseWriter, r *http.Request, slug string) {
	summary, err := session.Find(s.config.Workspace, slug)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.normalizeInteractivity(r.Context(), summary)
	data := pageData{
		BaseURL: s.config.BaseURL, Session: summary,
		MaxMessageBytes: session.MaxMessageBytes,
	}
	data.Repositories = s.repositories(r.Context(), summary)
	data.Plan = s.renderMarkdown(summary, "plan.md")
	data.State = s.renderMarkdown(summary, "state.md")
	s.render(w, "session", data)
}

func (s *Server) repositories(ctx context.Context, summary *session.Summary) []repository.Status {
	s.repositoryMu.Lock()
	if cached, ok := s.repositoryCache[summary.Slug]; ok && time.Since(cached.created) < time.Minute &&
		cached.updated.Equal(summary.ManifestUpdatedAt) && cached.closed == summary.Closed && cached.archived == summary.Archived {
		result := append([]repository.Status(nil), cached.statuses...)
		s.repositoryMu.Unlock()
		return result
	}
	s.repositoryMu.Unlock()
	inspectionContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	statuses := s.repository.Inspect(inspectionContext, summary.Repositories, summary.Closed)
	s.repositoryMu.Lock()
	s.repositoryCache[summary.Slug] = cachedRepositories{
		statuses: append([]repository.Status(nil), statuses...), created: time.Now(),
		updated: summary.ManifestUpdatedAt, closed: summary.Closed, archived: summary.Archived,
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
	creationDate := strings.TrimSpace(r.FormValue("creation_date"))
	if !session.ValidSlug(name) || len(name) > 48 {
		s.writeError(w, r, http.StatusBadRequest, "session name is invalid")
		return
	}
	if goal == "" || len([]byte(goal)) > session.MaxMessageBytes {
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
	slug := creationDate + "-" + name
	goalFile, err := os.CreateTemp("", "workspace-portal-goal-*.txt")
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	goalPath := goalFile.Name()
	defer os.Remove(goalPath)
	if err := goalFile.Chmod(0o600); err != nil {
		goalFile.Close()
		s.writeError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := goalFile.WriteString(goal); err != nil {
		goalFile.Close()
		s.writeError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if err := goalFile.Close(); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	devSession := s.config.DevSession
	if devSession == "" {
		devSession = filepath.Join(s.config.Workspace, "bin", "dev-session")
	}
	args := []string{
		"--require-runtime", "--workspace", s.config.Workspace,
		"--portal-base-url", s.config.BaseURL,
	}
	if s.config.AuthorityDir != "" {
		args = append(args, "--authority-dir", s.config.AuthorityDir)
	}
	if s.config.TmuxSocket != "" {
		args = append(args, "--tmux-socket", s.config.TmuxSocket)
	}
	if s.config.CodexSocket != "" {
		args = append(args, "--codex-socket", s.config.CodexSocket)
	}
	if s.config.CodexVersion != "" {
		args = append(args, "--codex-version", s.config.CodexVersion)
	}
	if s.config.CodexCommand != "" {
		args = append(args, "--codex-command", s.config.CodexCommand)
	}
	if s.config.PortalCommand != "" {
		args = append(args, "--portal-command", s.config.PortalCommand)
	}
	args = append(args, "start", slug, "--as-is", "--exclusive", "--no-attach", "--goal-file", goalPath, "--json")
	// Creation is journaled by dev-session and must be allowed to finish even if
	// the browser disconnects while waiting for the response.
	commandCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.Command(devSession, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = runProcessGroup(commandCtx, command)
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
		}
		s.writeError(w, r, http.StatusConflict, message)
		return
	}
	var result struct {
		Slug     string `json:"slug"`
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "dev-session returned invalid JSON: "+err.Error())
		return
	}
	http.Redirect(w, r, "/"+result.Slug+"/", http.StatusSeeOther)
}

func runProcessGroup(ctx context.Context, command *exec.Cmd) error {
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}

func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	remainder := strings.TrimPrefix(r.URL.Path, "/artifacts/")
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
	extension := strings.ToLower(filepath.Ext(parts[1]))
	contentTypes := map[string]string{
		".gif": "image/gif", ".jpeg": "image/jpeg", ".jpg": "image/jpeg",
		".json": "application/json", ".log": "text/plain; charset=utf-8",
		".md": "text/markdown; charset=utf-8", ".png": "image/png",
		".txt": "text/plain; charset=utf-8", ".webp": "image/webp",
		".yaml": "application/yaml", ".yml": "application/yaml",
	}
	contentType, allowed := contentTypes[extension]
	if !allowed {
		http.Error(w, "artifact type is not available", http.StatusUnsupportedMediaType)
		return
	}
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", info.Name()))
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (s *Server) sessionAPI(w http.ResponseWriter, r *http.Request) {
	remainder := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	parts := strings.Split(strings.Trim(remainder, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	summary, err := session.Find(s.config.Workspace, parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.normalizeInteractivity(r.Context(), summary)
	if summary.Codex.ThreadID == "" {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "session has no Codex thread"})
		return
	}
	if r.Method == http.MethodPost {
		lock, err := session.LockRuntimeShared(s.config.AuthorityDir, summary.Slug)
		if err != nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		defer lock.Close()
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
	threadID := summary.Codex.ThreadID
	switch {
	case r.Method == http.MethodGet && parts[1] == "thread":
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if s.config.ReadThread == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Codex transcript service is unavailable"})
			return
		}
		result, err := s.config.ReadThread(ctx, threadID)
		if err != nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, result)
	case r.Method == http.MethodGet && parts[1] == "events":
		if !summary.Interactive {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "session is not interactive"})
			return
		}
		s.events(w, r, threadID)
	case r.Method == http.MethodGet && parts[1] == "pending":
		if !summary.Interactive {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "session is not interactive"})
			return
		}
		s.pending(w, r, threadID)
	case r.Method == http.MethodPost && parts[1] == "message":
		var body struct {
			Message string `json:"message"`
		}
		if !s.decodeJSON(w, r, &body) {
			return
		}
		body.Message = strings.TrimSpace(body.Message)
		if body.Message == "" || len([]byte(body.Message)) > session.MaxMessageBytes {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf(
					"message must contain between 1 and %s bytes",
					session.FormattedMaxMessageBytes(),
				),
			})
			return
		}
		messageLock := s.messageLock(threadID)
		messageLock.Lock()
		defer messageLock.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if err := s.config.Codex.Send(ctx, threadID, body.Message); err != nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	case r.Method == http.MethodPost && parts[1] == "interrupt":
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := s.config.Codex.Interrupt(ctx, threadID); err != nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	case r.Method == http.MethodPost && parts[1] == "respond":
		s.respond(w, r, threadID)
	default:
		http.NotFound(w, r)
	}
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
	if summary.Closed || !creationReady || s.config.AuthorityDir == "" {
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

func (s *Server) messageLock(threadID string) *sync.Mutex {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	lock := s.messageLocks[threadID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.messageLocks[threadID] = lock
	}
	return lock
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, threadID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	events, unsubscribe, err := s.config.Codex.Subscribe(r.Context(), threadID)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stopping:
			return
		case <-keepalive.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		case _, ok := <-events:
			if !ok {
				return
			}
			_, _ = io.WriteString(w, "data: update\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) pending(w http.ResponseWriter, r *http.Request, threadID string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	prompts, err := s.config.Codex.PromptsWithItems(ctx, threadID)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if prompts == nil {
		prompts = []codex.Prompt{}
	}
	s.writeJSON(w, http.StatusOK, prompts)
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request, threadID string) {
	var body struct {
		ID       string                         `json:"id"`
		Decision string                         `json:"decision"`
		Answers  map[string]map[string][]string `json:"answers"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var err error
	if len(body.Answers) > 0 {
		err = s.config.Codex.RespondAnswers(ctx, body.ID, threadID, body.Answers)
	} else {
		err = s.config.Codex.RespondDecision(ctx, body.ID, threadID, body.Decision)
	}
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) renderMarkdown(summary *session.Summary, path string) template.HTML {
	file, _, err := session.OpenArtifact(summary, path, 1024*1024)
	if err != nil {
		return template.HTML("<p class=\"empty\">Not available.</p>")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return template.HTML("<p class=\"notice error\">Unable to read document.</p>")
	}
	var output strings.Builder
	if err := s.markdown.Convert(data, &output); err != nil {
		return template.HTML("<p class=\"notice error\">Unable to render document.</p>")
	}
	return template.HTML(s.sanitizer.Sanitize(output.String())) // #nosec G203 -- sanitized by bluemonday.
}

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	s.renderStatus(w, http.StatusOK, name, data)
}
func (s *Server) renderStatus(w http.ResponseWriter, status int, name string, data pageData) {
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
