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
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var generatedSessionName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+){2,5}$`)

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

func fallbackSessionName(raw string) string {
	line := ""
	for _, candidate := range strings.Split(raw, "\n") {
		if strings.TrimSpace(candidate) != "" {
			line = candidate
			break
		}
	}
	folded := norm.NFD.String(strings.ToLower(line))
	folded = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return ' '
	}, folded)
	words := strings.Fields(folded)
	if len(words) > 6 {
		words = words[:6]
	}
	name := boundedSessionWords(strings.Join(words, "-"), 48)
	if name == "" {
		return "session"
	}
	return name
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

func (s *Server) namePreparation(ctx context.Context, raw string) (string, string, error) {
	if strings.TrimSpace(raw) == "" {
		return "session", "attachment_only", nil
	}
	started := time.Now()
	budget, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, outcome := "", "unavailable"
	if s.config.SessionNamer != nil {
		select {
		case s.namingSlots <- struct{}{}:
			defer func() { <-s.namingSlots }()
		case <-budget.Done():
			if ctx.Err() != nil {
				return "", "", ctx.Err()
			}
			s.config.Logger.Printf("session naming duration_ms=%d outcome=timeout", time.Since(started).Milliseconds())
			return fallbackSessionName(raw), "timeout", nil
		}
		output, err := s.config.SessionNamer(budget, namingPrompt(raw))
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		if budget.Err() != nil {
			outcome = "timeout"
		} else if err != nil {
			outcome = "utility_error"
		} else {
			result, err = parseSessionName(output)
			if err != nil {
				outcome = "invalid_output"
			} else {
				outcome = "model"
			}
		}
	}
	if ctx.Err() != nil {
		return "", "", ctx.Err()
	}
	if result == "" {
		result = fallbackSessionName(raw)
	}
	s.config.Logger.Printf("session naming duration_ms=%d outcome=%s", time.Since(started).Milliseconds(), outcome)
	return result, outcome, nil
}
