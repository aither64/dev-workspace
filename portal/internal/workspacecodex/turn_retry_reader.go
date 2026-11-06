package workspacecodex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

const retryItemPageSize = 64

// All fields are disposable acceleration hints. A new reader recomputes the
// entire durable prefix; an active tail always rechecks its last bounded page.
type retryProofReader struct {
	key, cursor, previousCursor, lastItem string
	count                                 uint64
	digest, lastUser                      string
	path                                  string
	file                                  os.FileInfo
	tail                                  string
}

type retryNativeItem struct {
	TurnID string          `json:"turnId"`
	Item   json.RawMessage `json:"item"`
}

func retryProofKey(s *turnretry.State, turn string) string {
	data, _ := json.Marshal([]any{s.Scope, s.Epoch, s.Binding, s.ManagedTurn, turn})
	return textHash(string(data))
}

func retryRolloutTail(path string, size int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	start := max(int64(0), size-4096)
	data := make([]byte, size-start)
	_, err = file.ReadAt(data, start)
	if err != nil {
		return "", err
	}
	return textHash(string(data)), nil
}

func (c *Client) forgetRetryReader(thread string) {
	c.retryProofMu.Lock()
	delete(c.retryProofReaders, thread)
	c.retryProofMu.Unlock()
}

func (c *Client) keepRetryReader(thread string, reader retryProofReader) {
	c.retryProofMu.Lock()
	defer c.retryProofMu.Unlock()
	if c.retryProofReaders == nil {
		c.retryProofReaders = map[string]retryProofReader{}
	}
	if len(c.retryProofReaders) >= 128 {
		for key := range c.retryProofReaders {
			delete(c.retryProofReaders, key)
			break
		}
	}
	c.retryProofReaders[thread] = reader
}

// AdvanceProof persists source removal and prefix extension before publishing a
// cursor hint. A failed save invalidates the hint, including post-rename errors.
func (b retryBackend) AdvanceProof(ctx context.Context, scope turnretry.Scope, tx *turnretry.Transaction, turn string) error {
	if turn == "" || tx.State.Handled == turn || tx.State.Uncertain() {
		return nil
	}
	if legacyRetryProof(&tx.State, turn) {
		return nil
	}
	before := tx.State
	reader, err := b.client.advanceRetryPrefix(ctx, &tx.State, turn)
	latest, readErr := b.client.Client.ReadTurnState(ctx, scope.Thread)
	if readErr != nil || latest.LatestTurnID != turn {
		tx.State = before
		b.client.forgetRetryReader(scope.Thread)
		return turnretry.ErrProofIncomplete
	}
	if saveErr := tx.Save(); saveErr != nil {
		b.client.forgetRetryReader(scope.Thread)
		return saveErr
	}
	if err == nil || errors.Is(err, turnretry.ErrProofIncomplete) {
		reader.key = retryProofKey(&tx.State, turn)
		b.client.keepRetryReader(scope.Thread, reader)
	} else {
		b.client.forgetRetryReader(scope.Thread)
	}
	return err
}

func legacyRetryProof(state *turnretry.State, turn string) bool {
	if state.ManagedTurn != nil || state.Proof(turn) == nil {
		return false
	}
	unproven, err := state.HasUnprovenSnapshots(turn)
	return err == nil && !unproven
}

func (c *Client) advanceRetryPrefix(ctx context.Context, state *turnretry.State, turn string) (retryProofReader, error) {
	scope, binding := state.Scope, state.Binding
	if state.Handled == turn || binding == nil || binding.Epoch != state.Epoch || c.RetryPolicyResolver == nil {
		return retryProofReader{}, &turnretry.ProofFailure{Stage: "binding_unavailable", Err: turnretry.ErrMissingProof}
	}
	epoch, err := session.SocketIdentity(scope.Socket)
	if err != nil || epoch != binding.SocketIdentity {
		return retryProofReader{}, &turnretry.ProofFailure{Stage: "binding_unavailable", Err: turnretry.ErrMissingProof}
	}
	policy, err := c.RetryPolicyResolver(ctx, scope.Thread)
	if err != nil || !reflect.DeepEqual(policy, binding.Policy) || binding.CommonInstructions != c.retryCommonInstructions {
		return retryProofReader{}, &turnretry.ProofFailure{Stage: "binding_unavailable", Err: turnretry.ErrMissingProof}
	}
	provider, err := c.retryProvider(ctx, scope)
	if err != nil || provider != binding.Provider {
		return retryProofReader{}, &turnretry.ProofFailure{Stage: "binding_unavailable", Err: turnretry.ErrMissingProof}
	}
	metadata, err := c.Client.ReadThreadMetadata(ctx, scope.Thread, true)
	if err != nil {
		return retryProofReader{}, err
	}
	if metadata.ID != scope.Thread || metadata.Cwd != filepath.Join(scope.Workspace, "work", scope.Slug) || metadata.Path == nil {
		return retryProofReader{}, &turnretry.ProofFailure{Stage: "binding_unavailable", Err: turnretry.ErrMissingProof}
	}
	path := *metadata.Path
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return retryProofReader{}, &turnretry.ProofFailure{Stage: "binding_unavailable", Err: turnretry.ErrMissingProof}
	}
	seed := turnretry.ManagedTurnSeed(scope, state.Epoch, binding.Digest, turn)
	c.retryProofMu.Lock()
	reader, cached := c.retryProofReaders[scope.Thread]
	c.retryProofMu.Unlock()
	if cached {
		tail, tailErr := retryRolloutTail(path, reader.file.Size())
		cached = reader.key == retryProofKey(state, turn) && reader.path == path && os.SameFile(reader.file, info) &&
			info.Size() >= reader.file.Size() && (info.Size() != reader.file.Size() || info.ModTime().Equal(reader.file.ModTime())) && tailErr == nil && tail == reader.tail
	}
	if !cached {
		reader = retryProofReader{digest: seed}
	}
	reader.path, reader.file = path, info
	reader.tail, err = retryRolloutTail(path, info.Size())
	if err != nil {
		return reader, err
	}
	target := turnretry.ManagedTurn{Turn: turn, Digest: seed}
	if state.ManagedTurn != nil && state.ManagedTurn.Turn == turn {
		target = *state.ManagedTurn
	}
	budget := c.retryProofPageBudget
	if budget <= 0 {
		budget = 8
	}
	deadline := time.Now().Add(time.Second)
	for pageIndex := 0; pageIndex < budget; pageIndex++ {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return reader, turnretry.ErrProofIncomplete
		}
		beforePage := reader
		params := map[string]any{"threadId": scope.Thread, "turnId": turn, "sortDirection": "asc", "limit": retryItemPageSize}
		if reader.cursor != "" {
			params["cursor"] = reader.cursor
		}
		var page struct {
			Data       *[]retryNativeItem `json:"data"`
			NextCursor *string            `json:"nextCursor"`
		}
		if err := c.Request(ctx, "thread/items/list", params, &page); err != nil {
			return reader, &turnretry.ProofFailure{Stage: "history_read_failed", Err: err}
		}
		if page.Data == nil || len(*page.Data) > retryItemPageSize || len(*page.Data) == 0 && page.NextCursor != nil {
			return reader, &turnretry.ProofFailure{Stage: "history_invalid", Err: turnretry.ErrMissingProof}
		}
		if page.NextCursor != nil {
			next := *page.NextCursor
			if next == "" || len(next) > 4096 || next == reader.cursor || next == reader.previousCursor {
				return reader, &turnretry.ProofFailure{Stage: "history_invalid", Err: turnretry.ErrMissingProof}
			}
		}
		seen := map[string]bool{}
		for _, entry := range *page.Data {
			var item struct {
				ID, Type string
				ClientID *string `json:"clientId"`
				Content  *[]struct {
					Type string
					Text *string
				}
			}
			if json.Unmarshal(entry.Item, &item) != nil || entry.TurnID != turn || item.ID == "" || len(item.ID) > 4096 || item.Type == "" || seen[item.ID] || reader.lastItem == item.ID {
				return reader, &turnretry.ProofFailure{Stage: "history_invalid", Err: turnretry.ErrMissingProof}
			}
			seen[item.ID], reader.lastItem = true, item.ID
			if item.Type != "userMessage" {
				continue
			}
			if item.Content == nil {
				return reader, &turnretry.ProofFailure{Stage: "history_invalid", Err: turnretry.ErrMissingProof}
			}
			parts := []string{}
			for _, input := range *item.Content {
				if input.Type != "text" || input.Text == nil {
					return reader, &turnretry.ProofFailure{Stage: "history_invalid", Err: turnretry.ErrMissingProof}
				}
				parts = append(parts, *input.Text)
			}
			client := ""
			if item.ClientID != nil {
				client = *item.ClientID
			}
			digest := textHash(strings.Join(parts, "\n"))
			if reader.count < target.Count {
				reader.count++
				reader.digest, reader.lastUser = turnretry.ManagedInputDigest(reader.digest, item.ID, client, digest), item.ID
				if reader.count == target.Count && (reader.digest != target.Digest || reader.lastUser != target.LastUserItemID) {
					return reader, &turnretry.ProofFailure{Stage: "history_invalid", Err: turnretry.ErrMissingProof}
				}
				continue
			}
			if err := state.CompactManagedInput(turn, item.ID, client, digest); err != nil {
				return reader, &turnretry.ProofFailure{Stage: "input_unmatched", Err: err}
			}
			reader.count++
			reader.digest, reader.lastUser = state.ManagedTurn.Digest, item.ID
		}
		if page.NextCursor == nil {
			expected := seed
			if state.ManagedTurn != nil && state.ManagedTurn.Turn == turn {
				expected = state.ManagedTurn.Digest
			}
			if reader.count < target.Count || reader.digest != expected {
				return reader, &turnretry.ProofFailure{Stage: "history_invalid", Err: turnretry.ErrMissingProof}
			}
			latest, err := c.Client.ReadTurnState(ctx, scope.Thread)
			if err != nil || latest.LatestTurnID != turn {
				return reader, turnretry.ErrProofIncomplete
			}
			// Revisit the input cursor of this bounded tail, verifying it against
			// the saved prefix before folding anything appended while active.
			reader = beforePage
			unproven, err := state.HasUnprovenSnapshots(turn)
			if err != nil {
				return reader, err
			}
			if unproven {
				return reader, turnretry.ErrProofIncomplete
			}
			return reader, nil
		}
		next := *page.NextCursor
		reader.previousCursor, reader.cursor = reader.cursor, next
	}
	return reader, turnretry.ErrProofIncomplete
}
