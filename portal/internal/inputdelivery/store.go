package inputdelivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime/trace"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	_ "modernc.org/sqlite"
)

type Gate func(context.Context, func() error) error

var ErrDeferredClose = errors.New("delivery database close deferred to process exit")

type Store struct {
	db              *sql.Conn
	pool            *sql.DB
	workspace, path string
	gate            Gate
	mu              sync.Mutex
	closed          bool
	closeErr        error
	stopCtx         context.Context
	stop            context.CancelFunc
	active          sync.WaitGroup
}

func (s *Store) access(ctx context.Context, run func(context.Context) error) error {
	registration := trace.StartRegion(ctx, "pi.delivery.store.registration_wait")
	s.mu.Lock()
	registration.End()
	if s.closed || s.stopCtx.Err() != nil {
		s.mu.Unlock()
		return sql.ErrConnDone
	}
	s.active.Add(1)
	s.mu.Unlock()
	defer s.active.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.stopCtx, cancel)
	defer func() { stop(); cancel() }()
	// End gate admission at callback entry, including the refusal path where
	// the gate never calls back. SQL time is not transition-lock wait time.
	admission := trace.StartRegion(ctx, "pi.delivery.store.gate_admission")
	err := s.gate(ctx, func() error {
		if admission != nil {
			admission.End()
			admission = nil
		}
		connection := trace.StartRegion(ctx, "pi.delivery.store.connection_wait")
		s.mu.Lock()
		connection.End()
		defer s.mu.Unlock()
		if s.closed {
			return sql.ErrConnDone
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		defer trace.StartRegion(ctx, "pi.delivery.store.sql_callback").End()
		return run(ctx)
	})
	if admission != nil {
		admission.End()
	}
	return err
}

func Path(root, workspace string) string {
	return filepath.Join(userstate.WorkspaceDirectory(root, "portal", workspace), "input-delivery.sqlite")
}

func private(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || directory && !info.IsDir() || !directory && !info.Mode().IsRegular() || directory && info.Mode().Perm() != 0700 || !directory && info.Mode().Perm() != 0600 {
		return ErrInvalid
	}
	return nil
}
func ensureDirectory(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return ErrInvalid
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return ErrInvalid
	}
	if err := ensureDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	f, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Open initializes only this workspace's private queue. Inspect never calls it.
func Open(ctx context.Context, root, workspace string, gate Gate) (*Store, error) {
	if gate == nil || !filepath.IsAbs(root) || filepath.Clean(root) != root || !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return nil, ErrInvalid
	}
	stopCtx, stop := context.WithCancel(context.Background())
	s := &Store{workspace: workspace, path: Path(root, workspace), gate: gate, stopCtx: stopCtx, stop: stop}
	err := gate(ctx, func() error {
		dir := filepath.Dir(s.path)
		if err := ensureDirectory(dir); err != nil {
			return err
		}
		if err := private(dir, true); err != nil {
			return err
		}
		f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = d.Sync()
		d.Close()
		if err != nil {
			return err
		}
		if err = private(s.path, false); err != nil {
			return err
		}
		u := url.URL{Scheme: "file", Path: s.path}
		pool, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(1000)&_pragma=synchronous(FULL)&_pragma=journal_mode(WAL)")
		if err != nil {
			return err
		}
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)
		pool.SetConnMaxLifetime(0)
		pool.SetConnMaxIdleTime(0)
		conn, err := pool.Conn(ctx)
		if err != nil {
			pool.Close()
			return err
		}
		s.pool, s.db = pool, conn
		if err = s.initialize(ctx); err != nil {
			conn.Close()
			pool.Close()
			return err
		}
		return nil
	})
	if err != nil {
		stop()
		return nil, err
	}
	return s, nil
}

// Close is terminal: no further Store calls are accepted even if transition
// exclusion defers the actual SQLite close until OS process teardown.
func (s *Store) Close(ctx context.Context) error {
	s.stop()
	s.mu.Lock()
	if s.closed {
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	s.mu.Unlock()
	s.active.Wait()
	err := s.gate(ctx, func() error { s.mu.Lock(); defer s.mu.Unlock(); return errors.Join(s.db.Close(), s.pool.Close()) })
	if err != nil {
		err = errors.Join(ErrDeferredClose, err)
	}
	s.mu.Lock()
	s.closeErr = err
	s.mu.Unlock()
	return err
}

func (s *Store) initialize(ctx context.Context) error {
	var err error
	conn := s.db
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	var version int
	if err = conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != 1 {
		return ErrInvalid
	}
	if version == 0 {
		var tables int
		if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
			return err
		}
		if tables != 0 {
			return ErrInvalid
		}
		_, err = conn.ExecContext(ctx, `CREATE TABLE metadata (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL, workspace TEXT NOT NULL);
CREATE TABLE deliveries (scope TEXT NOT NULL, id TEXT NOT NULL, sequence INTEGER NOT NULL, phase TEXT NOT NULL, retired INTEGER NOT NULL CHECK(retired IN (0,1)), next_at INTEGER NOT NULL, admission BLOB NOT NULL, body BLOB NOT NULL, PRIMARY KEY(scope,id), UNIQUE(scope,sequence));
CREATE INDEX delivery_due ON deliveries(phase,next_at,scope,sequence);
CREATE INDEX delivery_scope_phase ON deliveries(scope,phase,sequence);
CREATE INDEX delivery_open_scope ON deliveries(scope,sequence) WHERE phase NOT IN ('sent','not_sent') AND retired=0;
CREATE INDEX delivery_open_due ON deliveries(next_at,scope,sequence) WHERE phase NOT IN ('sent','not_sent') AND retired=0;
CREATE INDEX delivery_recent ON deliveries(scope,sequence DESC) WHERE phase IN ('sent','not_sent') OR retired=1;
CREATE TABLE refusals (scope TEXT NOT NULL, id TEXT NOT NULL, body BLOB NOT NULL, PRIMARY KEY(scope,id));
PRAGMA user_version=1;`)
		if err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO metadata VALUES(1,1,?)", s.workspace); err != nil {
			return err
		}
	}
	var storedVersion int
	var workspace string
	if err = conn.QueryRowContext(ctx, "SELECT version,workspace FROM metadata WHERE id=1").Scan(&storedVersion, &workspace); err != nil {
		return err
	}
	if storedVersion != 1 || workspace != s.workspace {
		return ErrInvalid
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}
func (s *Store) checkMetadata(ctx context.Context) error {
	var version int
	var workspace string
	if err := s.db.QueryRowContext(ctx, "SELECT version,workspace FROM metadata WHERE id=1").Scan(&version, &workspace); err != nil {
		return err
	}
	if version != 1 || workspace != s.workspace {
		return ErrInvalid
	}
	return nil
}

func decode(data []byte) (Record, error) {
	var r Record
	if len(data) > 128<<10 {
		return r, ErrInvalid
	}
	if err := piquestions.Decode(data, &r); err != nil {
		return r, err
	}
	return r, r.Validate()
}
func (s *Store) get(ctx context.Context, scope, id string) (Record, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, "SELECT body FROM deliveries WHERE scope=? AND id=?", scope, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	r, err := decode(data)
	if err == nil && (r.Admission.Scope != scope || r.Status.ClientUserMessageID != id) {
		err = ErrInvalid
	}
	return r, err
}

// Admit holds no attachment/native lock and does no work proportional to history.
func (s *Store) admit(ctx context.Context, a Admission) (Record, error) {
	if len(a.Request.AttachmentIDs) == 0 {
		a.Request.AttachmentIDs = nil
	}
	if err := a.Validate(); err != nil {
		return Record{}, err
	}
	conn := s.db
	var err error
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Record{}, err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	if previous, e := s.resolve(ctx, a.Scope, a.Request); !errors.Is(e, ErrNotFound) {
		if e == nil && Digest(previous.Admission) != Digest(a) {
			return Record{}, ErrConflict
		}
		return previous, e
	}
	var count int
	if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM deliveries WHERE scope=? AND phase NOT IN ('sent','not_sent') AND retired=0", a.Scope).Scan(&count); err != nil {
		return Record{}, err
	}
	if count >= 20 {
		return Record{}, ErrCapacity
	}
	var sequence uint64
	if err = conn.QueryRowContext(ctx, "SELECT coalesce(max(sequence),0)+1 FROM deliveries WHERE scope=?", a.Scope).Scan(&sequence); err != nil {
		return Record{}, err
	}
	now := time.Now().UnixMilli()
	r := Record{Admission: a, Status: backend.DeliveryStatus{Version: 1, ClientUserMessageID: a.Request.ClientUserMessageID, Scope: a.Scope, Selection: a.Request.Selection, Delivery: a.Request.Delivery, Sequence: sequence, State: "queued", Message: a.Request.Message, AttachmentIDs: a.Request.AttachmentIDs, CreatedAtMS: now, UpdatedAtMS: now}, NextAttemptMS: now}
	body, _ := json.Marshal(r)
	immutable, _ := json.Marshal(a)
	_, err = conn.ExecContext(ctx, "INSERT INTO deliveries VALUES(?,?,?,?,?,?,?,?)", a.Scope, a.Request.ClientUserMessageID, sequence, "queued", 0, now, immutable, body)
	if err != nil {
		return Record{}, err
	}
	commit := trace.StartRegion(ctx, "pi.delivery.store.commit")
	_, err = conn.ExecContext(ctx, "COMMIT")
	if err == nil {
		// This event follows the actual durable COMMIT return, not HTTP
		// response publication or the record's wall-clock creation field.
		trace.Logf(ctx, "pi.delivery.commit", "originalID=%s phase=queued outcome=committed", a.Request.ClientUserMessageID)
	}
	commit.End()
	return r, err
}

func (s *Store) list(ctx context.Context, scope string) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM deliveries WHERE scope=? AND phase NOT IN ('sent','not_sent') AND retired=0 UNION ALL SELECT body FROM (SELECT body FROM deliveries WHERE scope=? AND (phase IN ('sent','not_sent') OR retired=1) ORDER BY sequence DESC LIMIT 100)`, scope, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		r, e := decode(data)
		if e != nil || r.Admission.Scope != scope {
			return nil, ErrInvalid
		}
		out = append(out, r)
		if len(out) > 120 {
			return nil, ErrInvalid
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Status.Sequence < out[j].Status.Sequence })
	return out, rows.Err()
}

// Update uses a phase CAS. A dispatch claim has exactly one winner; recovery has
// no transition back to queued, even when no socket write can be observed.
func (s *Store) update(ctx context.Context, r Record, from string) (bool, error) {
	if r.Validate() != nil {
		return false, ErrInvalid
	}
	allowed := from == "queued" && (r.Status.State == "queued" || r.Status.State == "dispatching" || r.Status.State == "not_sent") ||
		(from == "dispatching" || from == "pending" || from == "unknown") && (r.Status.State == "sent" || r.Status.State == "pending" || r.Status.State == "unknown" || r.Status.State == "not_sent")
	if !allowed {
		return false, ErrInvalid
	}
	r.Status.UpdatedAtMS = time.Now().UnixMilli()
	data, _ := json.Marshal(r)
	immutable, _ := json.Marshal(r.Admission)
	query := `UPDATE deliveries SET phase=?,retired=?,next_at=?,body=? WHERE scope=? AND id=? AND phase=? AND admission=? AND retired<=?`
	arguments := []any{r.Status.State, r.Status.Retired, r.NextAttemptMS, data, r.Admission.Scope, r.Status.ClientUserMessageID, from, immutable, r.Status.Retired}
	if r.Status.State == "dispatching" {
		query += ` AND NOT EXISTS(SELECT 1 FROM deliveries prior WHERE prior.scope=? AND prior.sequence<? AND prior.phase IN ('queued','dispatching','unknown') AND prior.retired=0)`
		arguments = append(arguments, r.Admission.Scope, r.Status.Sequence)
	}
	update := trace.StartRegion(ctx, "pi.delivery.store.update")
	result, err := s.db.ExecContext(ctx, query, arguments...)
	update.End()
	if err != nil {
		trace.Logf(ctx, "pi.delivery.cas", "originalID=%s from=%s to=%s outcome=sql_error", r.Status.ClientUserMessageID, from, r.Status.State)
		return false, err
	}
	n, err := result.RowsAffected()
	trace.Logf(ctx, "pi.delivery.cas", "originalID=%s from=%s to=%s won=%t error=%t", r.Status.ClientUserMessageID, from, r.Status.State, n == 1 && err == nil, err != nil)
	if err == nil && n == 1 && from == "queued" && r.Status.State == "dispatching" {
		// RowsAffected confirms the claim CAS after its autocommit UPDATE;
		// this is a post-commit upper boundary, not an exact lock grant.
		trace.Logf(ctx, "pi.delivery.claim", "originalID=%s phase=dispatching outcome=won_post_commit_upper", r.Status.ClientUserMessageID)
	}
	return n == 1, err
}

// Due returns one candidate per scope. Native admission/dispatch happens later,
// outside this read and outside every database transaction.
func (s *Store) due(ctx context.Context, after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 4 {
		return nil, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.body FROM deliveries d WHERE d.phase NOT IN ('sent','not_sent') AND d.retired=0 AND d.next_at<=? AND d.id=(SELECT p.id FROM deliveries p WHERE p.scope=d.scope AND p.phase NOT IN ('sent','not_sent') AND p.retired=0 AND p.next_at<=? ORDER BY CASE p.phase WHEN 'pending' THEN 1 ELSE 0 END,CASE p.phase WHEN 'pending' THEN p.next_at ELSE p.sequence END,p.sequence LIMIT 1) AND NOT EXISTS(SELECT 1 FROM deliveries p WHERE p.scope=d.scope AND p.phase IN ('unknown','dispatching') AND p.retired=0 AND p.sequence<d.sequence) ORDER BY d.scope<=?,d.scope LIMIT ?`, time.Now().UnixMilli(), time.Now().UnixMilli(), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		r, e := decode(data)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Inspect reads retained state without creating a database, sidecar or directory.
// The caller excludes writers through ordinary package-transition admission.
func inspect(ctx context.Context, root, workspace string, found func(Record), foundRefusal func(Refusal)) (int, error) {
	path := Path(root, workspace)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	if err := private(path, false); err != nil {
		return 0, err
	}
	if err := private(filepath.Dir(path), true); err != nil {
		return 0, err
	}
	snapshot, err := os.MkdirTemp("", "input-delivery-inspect-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(snapshot)
	for _, suffix := range []string{"", "-wal"} {
		source := path + suffix
		if _, e := os.Lstat(source); errors.Is(e, os.ErrNotExist) && suffix != "" {
			continue
		} else if e != nil {
			return 0, e
		}
		if e := private(source, false); e != nil {
			return 0, e
		}
		if e := copySnapshot(ctx, source, filepath.Join(snapshot, "queue.sqlite"+suffix)); e != nil {
			return 0, e
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(snapshot, "queue.sqlite")}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	s := Store{db: conn, workspace: workspace}
	if err = s.checkMetadata(ctx); err != nil {
		return 0, err
	}
	var version int
	if err = conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		return 0, ErrInvalid
	}
	rows, err := conn.QueryContext(ctx, "SELECT scope,id,sequence,phase,retired,admission,body FROM deliveries")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var scope, id, phase string
		var sequence uint64
		var retired bool
		var immutable, body []byte
		if err = rows.Scan(&scope, &id, &sequence, &phase, &retired, &immutable, &body); err != nil {
			return 0, err
		}
		r, e := decode(body)
		if e != nil || r.Admission.Selection.Handle.Conversation.Session.Workspace != workspace || r.Admission.Scope != scope || r.Status.ClientUserMessageID != id || r.Status.Sequence != sequence || r.Status.State != phase || r.Status.Retired != retired || Digest(r.Admission) != Digest(json.RawMessage(immutable)) {
			return 0, fmt.Errorf("invalid retained delivery: %w", ErrInvalid)
		}
		if found != nil {
			found(r)
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	negative, err := conn.QueryContext(ctx, "SELECT scope,id,body FROM refusals")
	if err != nil {
		return 0, err
	}
	defer negative.Close()
	for negative.Next() {
		var scope, id string
		var body []byte
		if err = negative.Scan(&scope, &id, &body); err != nil {
			return 0, err
		}
		refusal, e := decodeRefusal(body)
		if e != nil || refusal.Scope != scope || refusal.Request.ClientUserMessageID != id || refusal.Conversation.Session.Workspace != workspace {
			return 0, ErrInvalid
		}
		if _, e = s.get(ctx, scope, id); !errors.Is(e, ErrNotFound) {
			return 0, ErrInvalid
		}
		if foundRefusal != nil {
			foundRefusal(refusal)
		}
		count++
	}
	return count, negative.Err()
}

func Inspect(ctx context.Context, root, workspace string) (int, error) {
	return inspect(ctx, root, workspace, nil, nil)
}
func InspectConversations(ctx context.Context, root, workspace string) ([]agentbackend.ConversationRef, error) {
	refs := []agentbackend.ConversationRef{}
	seen := map[agentbackend.ConversationRef]bool{}
	_, err := inspect(ctx, root, workspace, func(r Record) {
		ref := r.Admission.Selection.Handle.Conversation
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}, func(r Refusal) {
		if !seen[r.Conversation] {
			seen[r.Conversation] = true
			refs = append(refs, r.Conversation)
		}
	})
	return refs, err
}

// InspectAll also refuses orphan queue databases and sidecars. The namespace
// mapping is the existing portal mapping, never inferred from database content.
func InspectAll(ctx context.Context, root string, workspaces []string) ([]agentbackend.ConversationRef, error) {
	owners := map[string]string{}
	for _, workspace := range workspaces {
		owners[filepath.Base(filepath.Dir(Path(root, workspace)))] = workspace
	}
	entries, err := os.ReadDir(filepath.Join(root, "portal"))
	if errors.Is(err, os.ErrNotExist) {
		return []agentbackend.ConversationRef{}, nil
	}
	if err != nil {
		return nil, err
	}
	refs := []agentbackend.ConversationRef{}
	for _, entry := range entries {
		dir := filepath.Join(root, "portal", entry.Name())
		present := false
		database := false
		names, e := os.ReadDir(dir)
		if errors.Is(e, syscall.ENOTDIR) {
			continue
		}
		if e != nil {
			return nil, e
		}
		for _, name := range names {
			if strings.HasPrefix(name.Name(), "input-delivery.sqlite") {
				if name.Name() != "input-delivery.sqlite" && name.Name() != "input-delivery.sqlite-wal" && name.Name() != "input-delivery.sqlite-shm" {
					return nil, ErrInvalid
				}
				if err := private(filepath.Join(dir, name.Name()), false); err != nil {
					return nil, err
				}
			}
		}
		for _, name := range []string{"input-delivery.sqlite", "input-delivery.sqlite-wal", "input-delivery.sqlite-shm"} {
			if _, e := os.Lstat(filepath.Join(dir, name)); e == nil {
				present = true
				if name == "input-delivery.sqlite" {
					database = true
				}
			} else if !errors.Is(e, os.ErrNotExist) && !errors.Is(e, syscall.ENOTDIR) {
				return nil, e
			}
		}
		if !present {
			continue
		}
		workspace := owners[entry.Name()]
		if workspace == "" || !database {
			return nil, ErrInvalid
		}
		found, e := InspectConversations(ctx, root, workspace)
		if e != nil {
			return nil, e
		}
		refs = append(refs, found...)
	}
	return refs, nil
}

func (s *Store) Get(ctx context.Context, scope, id string) (r Record, err error) {
	err = s.access(ctx, func(ctx context.Context) error { var e error; r, e = s.get(ctx, scope, id); return e })
	return
}
func (s *Store) Admit(ctx context.Context, a Admission) (r Record, err error) {
	err = s.access(ctx, func(ctx context.Context) error { var e error; r, e = s.admit(ctx, a); return e })
	return
}
func (s *Store) List(ctx context.Context, scope string) (r []Record, err error) {
	err = s.access(ctx, func(ctx context.Context) error { var e error; r, e = s.list(ctx, scope); return e })
	return
}
func (s *Store) Update(ctx context.Context, r Record, from string) (won bool, err error) {
	err = s.access(ctx, func(ctx context.Context) error { var e error; won, e = s.update(ctx, r, from); return e })
	return
}
func (s *Store) Due(ctx context.Context, after string, limit int) (r []Record, err error) {
	err = s.access(ctx, func(ctx context.Context) error { var e error; r, e = s.due(ctx, after, limit); return e })
	return
}

func copySnapshot(ctx context.Context, source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := in.Read(buffer)
		if n > 0 {
			if _, err = out.Write(buffer[:n]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return out.Close()
		}
		if readErr != nil {
			return readErr
		}
	}
}
