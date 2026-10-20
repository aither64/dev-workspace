package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aither64/codex-web/codex"
)

var generatedSessionName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+){1,5}$`)

var errSessionNaming = errors.New("Automatic naming failed. Retry naming or enter a custom name.")
var errSessionNamingFallback = errors.New("automatic naming unavailable; use a neutral name")

// The adapter supplies exactly one JSON object. It owns model selection and
// isolation; no caller may derive utility settings from a development team.
func parseSessionName(output string) (string, error) {
	if len(output) > 256 || !utf8.ValidString(output) {
		return "", errors.New("invalid naming output")
	}
	var object map[string]json.RawMessage
	if err := rejectDuplicateCreationJSONKeys([]byte(output)); err != nil {
		return "", err
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	if err := decoder.Decode(&object); err != nil {
		return "", err
	}
	if decoder.Decode(&struct{}{}) != io.EOF || len(object) != 1 || object["name"] == nil {
		return "", errors.New("invalid naming object")
	}
	var name string
	if json.Unmarshal(object["name"], &name) != nil || len(name) > 48 || !generatedSessionName.MatchString(name) {
		return "", errors.New("invalid generated name")
	}
	return name, nil
}

func boundedSessionWords(name string, limit int) string {
	result := ""
	for _, word := range strings.Split(name, "-") {
		candidate := word
		if result != "" {
			candidate = result + "-" + word
		}
		if len(candidate) > limit {
			break
		}
		result = candidate
	}
	return result
}

func suffixedSessionName(base string, number int) string {
	if number == 1 {
		return base
	}
	suffix := "-" + strconv.Itoa(number)
	prefix := boundedSessionWords(base, 48-len(suffix))
	if prefix == "" {
		prefix = "session"
	}
	return prefix + suffix
}

func namingPrompt(raw string) string {
	if len(raw) <= 8192 {
		return raw
	}
	cut := 8192
	for !utf8.ValidString(raw[:cut]) {
		cut--
	}
	return raw[:cut]
}

func (s *Server) namePreparation(ctx context.Context, raw, provider string) (string, string, error) {
	if strings.TrimSpace(raw) == "" {
		return "session", "attachment_only", nil
	}
	started := time.Now()
	budget, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, outcome, category := "", "unavailable", "unavailable"
	if s.config.SessionNamer != nil {
		select {
		case s.namingSlots <- struct{}{}:
			defer func() { <-s.namingSlots }()
		case <-budget.Done():
			if ctx.Err() != nil {
				return "", "", ctx.Err()
			}
			s.config.Logger.Printf("session naming duration_ms=%d outcome=timeout category=queue", time.Since(started).Milliseconds())
			return "development-session", "timeout", nil
		}
		output, err := s.config.SessionNamer(budget, namingPrompt(raw), provider)
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		if budget.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			outcome, category = "timeout", "deadline"
			if errors.Is(err, errSessionNamingFallback) {
				result = "development-session"
			}
		} else if err != nil {
			outcome = "utility_error"
			category = sessionNamingErrorCategory(err)
			if errors.Is(err, errSessionNamingFallback) {
				result = "development-session"
			}
		} else {
			result, err = parseSessionName(output)
			if err != nil {
				outcome, category = "invalid_output", "output"
			} else {
				outcome, category = "model", "ok"
			}
		}
	}
	if ctx.Err() != nil {
		return "", "", ctx.Err()
	}
	s.config.Logger.Printf("session naming duration_ms=%d outcome=%s category=%s", time.Since(started).Milliseconds(), outcome, category)
	if result == "" {
		return "", outcome, errSessionNaming
	}
	return result, outcome, nil
}

// Only provider-owned categories cross this boundary, never remote diagnostics.
func sessionNamingErrorCategory(err error) string {
	switch {
	case errors.Is(err, codex.ErrEphemeralIsolation):
		return "isolation"
	case errors.Is(err, codex.ErrEphemeralSettings):
		return "settings"
	case errors.Is(err, codex.ErrEphemeralProtocol):
		return "protocol"
	case errors.Is(err, codex.ErrEphemeralServer):
		return "server"
	default:
		return "utility"
	}
}
