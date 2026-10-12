package workspacecodex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Held prompts are evidence, never a runnable queue. They survive revival and
// repeated archives in private storage beside the submission ledger.
type heldPrompts struct {
	Schema   int                        `json:"schema"`
	Thread   string                     `json:"thread_id"`
	Cwd      string                     `json:"cwd"`
	Entries  map[string]json.RawMessage `json:"entries"`
	Attempts map[string]json.RawMessage `json:"attempts"`
}

func (c *Client) QuiesceExplicitArchive(ctx context.Context, thread, cwd string) error {
	if err := c.VerifyThread(ctx, thread, cwd); err != nil {
		return err
	}
	if c.RecoveryRoot == "" || c.workspace == "" {
		return errors.New("private archive recovery storage is required")
	}
	workspaceID := sha256.Sum256([]byte(c.workspace))
	threadID := sha256.Sum256([]byte(thread))
	directory := filepath.Join(c.RecoveryRoot, "archived-prompts", hex.EncodeToString(workspaceID[:]), filepath.Base(cwd))
	if cwd != filepath.Join(c.workspace, "work", filepath.Base(cwd)) {
		return errors.New("explicit archive directory is outside the session")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	path := filepath.Join(directory, hex.EncodeToString(threadID[:])+".json")
	record := heldPrompts{Schema: 1, Thread: thread, Cwd: cwd, Entries: map[string]json.RawMessage{}, Attempts: map[string]json.RawMessage{}}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &record); err != nil {
			return err
		}
		if record.Schema != 1 || record.Thread != thread || record.Cwd != cwd || record.Entries == nil || record.Attempts == nil {
			return errors.New("held archive prompts changed identity")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Preserve this conversation's durable receipts before retirement clears them.
	if c.submissionLedgerPath != "" {
		data, err := os.ReadFile(c.submissionLedgerPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				return err
			}
			for _, key := range []string{"attempts", "sends", "deletions"} {
				if len(fields[key]) == 0 {
					continue
				}
				// Each field is a thread-to-attempt mapping.
				var scoped map[string]json.RawMessage
				if err := json.Unmarshal(fields[key], &scoped); err != nil {
					return err
				}
				if len(scoped[thread]) != 0 {
					record.Attempts[key] = scoped[thread]
				}
			}
		}
	}
	var cursor string
	seen := map[string]bool{}
	for {
		params := map[string]any{"threadId": thread, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       *[]json.RawMessage `json:"data"`
			NextCursor *string            `json:"nextCursor"`
		}
		if err := c.Request(ctx, "thread/queue/list", params, &page); err != nil {
			return err
		}
		if page.Data == nil {
			return errors.New("archive queue returned no data")
		}
		for _, entry := range *page.Data {
			var identity struct {
				ID       string `json:"id"`
				ClientID string `json:"clientUserMessageId"`
			}
			if err := json.Unmarshal(entry, &identity); err != nil {
				return err
			}
			if identity.ID == "" || identity.ClientID == "" {
				return errors.New("archive queue entry has no identity")
			}
			record.Entries[identity.ID] = entry
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
		if cursor == "" || seen[cursor] {
			return errors.New("archive queue cursor repeated")
		}
		seen[cursor] = true
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".held-prompts-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return err
	}
	// List again: a lost deletion reply is reconciled by queue absence on retry.
	queue, err := c.ListQueue(ctx, thread)
	if err != nil {
		return err
	}
	for _, entry := range queue {
		if _, saved := record.Entries[entry.ID]; !saved {
			return errors.New("queue changed during archive capture")
		}
		var response any
		if err := c.Request(ctx, "thread/queue/delete", map[string]any{"threadId": thread, "queuedSubmissionId": entry.ID}, &response); err != nil {
			return err
		}
	}
	var goal struct {
		Goal *struct {
			Thread string `json:"threadId"`
			Status string `json:"status"`
		} `json:"goal"`
	}
	if err := c.Request(ctx, "thread/goal/get", map[string]any{"threadId": thread}, &goal); err != nil {
		return err
	}
	if goal.Goal != nil && goal.Goal.Status == "active" {
		if goal.Goal.Thread != thread {
			return errors.New("archive goal changed conversation")
		}
		var response any
		if err := c.Request(ctx, "thread/goal/set", map[string]any{"threadId": thread, "status": "paused"}, &response); err != nil {
			return err
		}
	}
	metadata, err := c.ReadThreadMetadata(ctx, thread, true)
	if err != nil {
		return err
	}
	if metadata.Path == nil {
		return nil
	}
	materialized, err := c.HistoryMaterialized(ctx, thread, cwd)
	if err != nil {
		return err
	}
	if !materialized {
		return nil
	}
	active, err := c.ActiveTurnID(ctx, thread)
	if err != nil {
		return err
	}
	if active != "" {
		if err := c.Interrupt(ctx, thread); err != nil {
			return err
		}
	}
	for {
		if err := c.RequireThreadTurnsIdle(ctx, thread); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for explicit archive interruption: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Native utilities carry parent identity in the selected protocol. Only a
// proven descendant of this retained thread belongs to explicit archival.
func (c *Client) QuiesceNativeDescendants(ctx context.Context, root, cwd string) error {
	// The caller stops root first. Keep ancestry across samples until every
	// proven parent has stopped, then resample to catch children spawned while
	// interruption was in flight. Retirement happens only after that fixed point.
	parents := map[string]string{}
	quiesced := map[string]int{}
	var depth func(string, map[string]bool) int
	depth = func(id string, seen map[string]bool) int {
		if id == root {
			return 1
		}
		if seen[id] || parents[id] == "" {
			return 0
		}
		seen[id] = true
		n := depth(parents[id], seen)
		if n == 0 {
			return 0
		}
		return n + 1
	}
	for {
		ids, err := c.LoadedThreadIDs(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if id == root {
				continue
			}
			// The source is a string for ordinary threads.
			var raw struct {
				Thread map[string]json.RawMessage `json:"thread"`
			}
			if err := c.Request(ctx, "thread/read", map[string]any{"threadId": id, "excludeTurns": true}, &raw); err != nil {
				return err
			}
			var directory string
			if err := json.Unmarshal(raw.Thread["cwd"], &directory); err != nil {
				return err
			}
			if directory != cwd {
				continue
			}
			source := raw.Thread["source"]
			if len(source) == 0 || source[0] != '{' {
				continue
			}
			var native struct {
				SubAgent struct {
					Spawn struct {
						Parent string `json:"parent_thread_id"`
					} `json:"thread_spawn"`
				} `json:"subAgent"`
			}
			if err := json.Unmarshal(source, &native); err != nil {
				return err
			}
			parent := native.SubAgent.Spawn.Parent
			if parent != "" {
				if previous := parents[id]; previous != "" && previous != parent {
					return errors.New("native archive descendant changed parent")
				}
				parents[id] = parent
			}
		}
		var pending []string
		for id := range parents {
			if _, stopped := quiesced[id]; !stopped && depth(id, map[string]bool{}) > 0 {
				pending = append(pending, id)
			}
		}
		if len(pending) == 0 {
			break
		}
		sort.Slice(pending, func(i, j int) bool {
			return depth(pending[i], map[string]bool{}) < depth(pending[j], map[string]bool{})
		})
		for _, child := range pending {
			if err := c.QuiesceExplicitArchive(ctx, child, cwd); err != nil {
				return err
			}
			quiesced[child] = depth(child, map[string]bool{})
		}
	}
	var children []string
	for id := range quiesced {
		children = append(children, id)
	}
	sort.Slice(children, func(i, j int) bool { return quiesced[children[i]] > quiesced[children[j]] })
	for _, child := range children {
		if err := c.ArchiveThread(ctx, child); err != nil {
			return err
		}
		if err := c.ClearRetiredThreadAttempts(child); err != nil {
			return err
		}
	}
	return nil
}
