package inputdelivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
)

func decodeRefusal(data []byte) (Refusal, error) {
	var r Refusal
	if len(data) > 64<<10 {
		return r, ErrInvalid
	}
	if err := piquestions.Decode(data, &r); err != nil {
		return r, err
	}
	return r, r.Validate()
}

// resolve consults both immutable identities. Callers that may create either
// disposition run it inside BEGIN IMMEDIATE on the same retained connection.
func (s *Store) resolve(ctx context.Context, scope string, request backend.DeliveryRequest) (Record, error) {
	r, err := s.get(ctx, scope, request.ClientUserMessageID)
	if err == nil {
		if !SameRequest(r.Admission.Request, request) {
			return Record{}, ErrConflict
		}
		return r, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Record{}, err
	}
	var data []byte
	err = s.db.QueryRowContext(ctx, "SELECT body FROM refusals WHERE scope=? AND id=?", scope, request.ClientUserMessageID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	refusal, err := decodeRefusal(data)
	if err != nil {
		return Record{}, err
	}
	if refusal.Scope != scope || refusal.Request.ClientUserMessageID != request.ClientUserMessageID {
		return Record{}, ErrInvalid
	}
	if !SameRequest(refusal.Request, request) {
		return Record{}, ErrConflict
	}
	return Record{}, refusal.disposition()
}

func (s *Store) Resolve(ctx context.Context, scope string, request backend.DeliveryRequest) (r Record, err error) {
	err = s.access(ctx, func(ctx context.Context) error { var e error; r, e = s.resolve(ctx, scope, request); return e })
	return
}

// Reject commits only an authentic negative identity. It creates no work row,
// sequence, execution selection or native submission. A late original request
// must resolve this identity instead of entering the dispatcher.
func (s *Store) Reject(ctx context.Context, scope string, conversation agentbackend.ConversationRef, request backend.DeliveryRequest, code string) (r Record, err error) {
	refusal := Refusal{Version: 1, Scope: scope, Conversation: conversation, Request: request, Code: code, CreatedAtMS: time.Now().UnixMilli()}
	if err = refusal.Validate(); err != nil {
		return
	}
	err = s.access(ctx, func(ctx context.Context) error {
		if _, e := s.db.ExecContext(ctx, "BEGIN IMMEDIATE"); e != nil {
			return e
		}
		defer s.db.ExecContext(context.Background(), "ROLLBACK")
		var e error
		r, e = s.resolve(ctx, scope, request)
		if !errors.Is(e, ErrNotFound) {
			return e
		}
		data, e := json.Marshal(refusal)
		if e != nil {
			return e
		}
		if _, e = s.db.ExecContext(ctx, "INSERT INTO refusals VALUES(?,?,?)", scope, request.ClientUserMessageID, data); e != nil {
			return e
		}
		if _, e = s.db.ExecContext(ctx, "COMMIT"); e != nil {
			return e
		}
		return refusal.disposition()
	})
	return
}
