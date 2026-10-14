package web

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type attentionEvent struct {
	Version   int    `json:"version"`
	ID        string `json:"id"`
	Identity  string `json:"identity"`
	Revision  uint64 `json:"revision"`
	Slug      string `json:"slug"`
	Member    string `json:"member,omitempty"`
	Kind      string `json:"kind"`
	Route     string `json:"route"`
	ExpiresAt int64  `json:"expiresAt"`
}

type attentionObservation struct {
	Identity   string
	Slug       string
	RootThread string
	Lead       codex.ActivitySnapshot
	Members    []codex.ActivitySnapshot
	Requests   []attentionRequest
	Fresh      bool
	Eligible   bool
}

type attentionRequest struct{ Key, Member, Kind string }

// This is a baseline and an event ledger, not a transcript. A candidate retains
// the successful lead turn while peers settle; peer completion cannot create it.
type attentionBaseline struct {
	Identity      string          `json:"identity"`
	RootThread    string          `json:"rootThread"`
	LeadTurn      string          `json:"leadTurn"`
	ArmedTurn     string          `json:"armedTurn,omitempty"`
	CompletedTurn string          `json:"completedTurn,omitempty"`
	Requests      []string        `json:"requests"`
	Revision      uint64          `json:"revision"`
	ObservedAt    int64           `json:"observedAt"`
	Eligible      bool            `json:"eligible"`
	Fresh         bool            `json:"fresh"`
	Event         *attentionEvent `json:"event,omitempty"`
}

func attentionDigest(parts ...string) string {
	data, _ := json.Marshal(parts)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func reduceAttention(previous attentionBaseline, observation attentionObservation, now time.Time) (attentionBaseline, bool) {
	b := previous
	freshBaseline := b.Identity != observation.Identity || b.RootThread != observation.RootThread ||
		!b.Eligible || b.ObservedAt == 0 || now.UnixMilli()-b.ObservedAt > 90000
	b.Identity, b.RootThread, b.Eligible, b.Fresh = observation.Identity, observation.RootThread, observation.Eligible, observation.Fresh
	if !observation.Eligible {
		b.Event, b.ArmedTurn, b.Eligible = nil, "", false
		return b, false
	}
	if !observation.Fresh {
		return b, false
	}
	b.ObservedAt = observation.Lead.ObservedAtMS
	if b.ObservedAt == 0 {
		b.ObservedAt = now.UnixMilli()
	}
	turn := observation.Lead.CurrentTurnID
	if freshBaseline {
		// A gap cannot create a completion, but it cannot erase an event already
		// durably observed either. Delivery expiry is independent of unread state.
		knownCurrent := previous.Identity == observation.Identity && previous.RootThread == observation.RootThread &&
			previous.Event != nil && attentionRelevant(*previous.Event, observation)
		b.ArmedTurn, b.CompletedTurn, b.Event = "", "", nil
		if knownCurrent {
			b.Event = previous.Event
		}
		if observation.Lead.LatestTurnStatus == "completed" {
			b.CompletedTurn = turn
		} else if observation.Lead.LatestTurnStatus == "inProgress" {
			b.ArmedTurn = turn
		}
	} else if turn != b.LeadTurn {
		b.ArmedTurn, b.Event = turn, nil
	}
	if !freshBaseline && turn != "" && observation.Lead.LatestTurnStatus == "inProgress" {
		b.ArmedTurn = turn
	}
	b.LeadTurn = turn
	requests := append([]attentionRequest(nil), observation.Requests...)
	sort.Slice(requests, func(i, j int) bool { return requests[i].Key < requests[j].Key })
	keys := make([]string, 0, len(requests))
	seen := make(map[string]bool)
	for _, key := range previous.Requests {
		seen[key] = true
	}
	var newRequest *attentionRequest
	for i := range requests {
		keys = append(keys, requests[i].Key)
		if !seen[requests[i].Key] && newRequest == nil {
			newRequest = &requests[i]
		}
	}
	b.Requests = keys
	allIdle := observation.Lead.CurrentState == "idle"
	for _, member := range observation.Members {
		allIdle = allIdle && member.CurrentState == "idle"
	}
	if b.Event != nil {
		valid := b.Event.Kind == "done" && len(requests) == 0 && allIdle && observation.Lead.LatestTurnStatus == "completed"
		for _, request := range requests {
			valid = valid || b.Event.ID == attentionDigest(observation.Identity, request.Key)
		}
		if !valid {
			b.Event = nil
		}
	}
	emit := false
	if len(requests) > 0 {
		// Existing blocking input remains actionable in the page after visiting.
		selected := requests[0]
		if newRequest == nil && b.Event != nil && b.Event.Kind != "done" {
			return b, false
		}
		if newRequest != nil {
			selected = *newRequest
		}
		id := attentionDigest(observation.Identity, selected.Key)
		if b.Event == nil || b.Event.ID != id {
			b.Revision++
			b.Event = &attentionEvent{Version: 1, ID: id, Identity: observation.Identity, Revision: b.Revision,
				Slug: observation.Slug, Member: selected.Member, Kind: selected.Kind,
				Route: attentionRoute(observation.Slug, selected.Member), ExpiresAt: now.Add(15 * time.Minute).UnixMilli()}
			emit = !freshBaseline && newRequest != nil
		}
		return b, emit
	}
	if allIdle && turn != "" && observation.Lead.LatestTurnStatus == "completed" && b.ArmedTurn == turn && b.CompletedTurn != turn {
		b.CompletedTurn = turn
		b.Revision++
		b.Event = &attentionEvent{Version: 1, ID: attentionDigest(observation.Identity, "done", turn), Identity: observation.Identity,
			Revision: b.Revision, Slug: observation.Slug, Kind: "done", Route: attentionRoute(observation.Slug, ""), ExpiresAt: now.Add(5 * time.Minute).UnixMilli()}
		emit = true
	}
	return b, emit
}

func attentionRoute(slug, member string) string {
	if member != "" && member != "lead" {
		return "/" + slug + "/?member=" + member + "#codex"
	}
	return "/" + slug + "/#codex"
}

func (s *Server) observeAttention(summary session.Summary, now time.Time) attentionObservation {
	identity, err := session.ObservationIdentity(s.config.Workspace, summary.Slug,
		filepath.Join(s.config.Workspace, summary.Root, summary.Slug), summary.Codex.ThreadID)
	o := attentionObservation{Identity: identity, Slug: summary.Slug, RootThread: summary.Codex.ThreadID}
	if err != nil || s.activity == nil || !s.activity.owns(summary) || s.recoveryHeld(summary.Slug, summary.Codex.ThreadID) {
		return o
	}
	s.operationMu.Lock()
	receipt, found := s.creations[summary.Slug]
	s.operationMu.Unlock()
	if found && receipt.State != "ready" {
		return o
	}
	o.Eligible, o.Fresh = true, true
	threads := map[string]string{"lead": summary.Codex.ThreadID}
	roster, err := s.loadTeamRoster(&summary)
	if err != nil && (!os.IsNotExist(err) || (found && receipt.Schema == 3)) {
		o.Fresh = false
		return o
	}
	if roster != nil {
		for _, member := range roster.Members {
			if member.State == "ready" && member.RetireIntent == "" {
				if member.Thread == "" || s.recoveryHeld(summary.Slug, member.Thread) {
					o.Fresh = false
					return o
				}
				threads[member.Address] = member.Thread
			}
		}
	}
	prompts, ok := s.activity.observer.(interface{ Prompts(string) []codex.Prompt })
	if !ok {
		o.Fresh = false
		return o
	}
	for member, thread := range threads {
		s.activity.readMu.Lock()
		cached, found := s.activity.snapshots[thread]
		s.activity.readMu.Unlock()
		if !found || cached.value.ThreadID != thread || now.Sub(cached.at) > 40*time.Second ||
			cached.value.ObservedAtMS <= 0 || now.UnixMilli()-cached.value.ObservedAtMS > 40000 ||
			(cached.value.CurrentState != "idle" && cached.value.CurrentState != "working" && cached.value.CurrentState != "waiting") {
			o.Fresh = false
			continue
		}
		if member == "lead" {
			o.Lead = cached.value
		} else {
			o.Members = append(o.Members, cached.value)
		}
		for _, prompt := range prompts.Prompts(thread) {
			if !prompt.IsBlocking {
				continue
			}
			if prompt.TurnID == "" || prompt.ItemID == "" || prompt.ThreadID != thread {
				o.Fresh = false
				continue
			}
			kind := "approval"
			if prompt.Kind == "userInput" {
				kind = "input"
			}
			if cached.value.CurrentState != "waiting" || (cached.value.WaitReason != "userInput" && cached.value.WaitReason != "approval") {
				o.Fresh = false
				continue
			}
			o.Requests = append(o.Requests, attentionRequest{attentionDigest(thread, prompt.TurnID, prompt.ItemID, prompt.Method), member, kind})
		}
	}
	return o
}
