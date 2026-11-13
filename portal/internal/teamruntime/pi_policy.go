package teamruntime

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

var piMemberPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}[0-9]+$`)

func piText(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validPiHandle(h agentbackend.ExecutionHandle) bool {
	c := h.Conversation
	return c.Backend == agentbackend.Pi && filepath.IsAbs(c.Session.Workspace) &&
		filepath.Clean(c.Session.Workspace) == c.Session.Workspace && session.ValidSlug(c.Session.Slug) &&
		digestPattern.MatchString(c.Session.Incarnation) && digestPattern.MatchString(c.Incarnation) &&
		digestPattern.MatchString(h.Generation) && piAddress(c.Member) && piText(c.NativeID, 4096)
}

func ValidatePiHandle(h agentbackend.ExecutionHandle) error {
	if !validPiHandle(h) {
		return ErrPiPolicy
	}
	return nil
}

func piAddress(address string) bool {
	return address == "lead" || (len(address) <= 64 && piMemberPattern.MatchString(address))
}

func ValidatePiCaller(c PiCaller) error {
	if !validPiHandle(c.Handle) || c.Runtime.Session != c.Handle.Conversation.Session ||
		c.Runtime.Generation != c.Handle.Generation || !projectUUIDPattern.MatchString(c.Runtime.InstanceID) ||
		!projectUUIDPattern.MatchString(c.Runtime.RequestID) || !digestPattern.MatchString(c.Runtime.Digest) {
		return ErrPiPolicy
	}
	return nil
}

func ValidatePiMessage(m PiMessage) error {
	if !messageIDPattern.MatchString(m.ID) || !piAddress(m.To) || !piText(m.Text, 20_000) {
		return ErrPiPolicy
	}
	return nil
}

func validPiSettings(s piruntime.EffectiveSettings) bool {
	return s.Revision > 0 && digestPattern.MatchString(s.PolicyRevision) &&
		piText(s.Provider, 4096) && piText(s.AccountRef, 4096) && piText(s.Model, 4096) && piText(s.Thinking, 4096)
}

// CheckPiDelivery is called by the admission owner with current projections.
// It authorizes no loading, wakeup, queue, steering or implicit settings change.
func CheckPiDelivery(caller PiCaller, from, target PiParticipant) error {
	if ValidatePiCaller(caller) != nil || from.Handle != caller.Handle || !validPiHandle(target.Handle) ||
		target.Handle.Conversation.Session != caller.Runtime.Session || target.Handle.Generation != caller.Runtime.Generation ||
		from.Policy.Validate() != nil || target.Policy.Validate() != nil {
		return ErrPiPolicy
	}
	for _, p := range []PiParticipant{from, target} {
		if p.State != "ready" || !p.Persisted || p.Held || !p.Loaded || !p.SettingsApplied {
			return ErrPiHeld
		}
		if !validPiSettings(p.Settings) {
			return ErrPiUnavailable
		}
	}
	if target.Activity != "idle" {
		return ErrPiHeld
	}
	return nil
}
