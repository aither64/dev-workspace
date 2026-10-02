// Package creationprogress carries optional, ephemeral creation stages on stderr.
package creationprogress

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"
	"unicode/utf8"
)

const Environment = "DEV_WORKSPACE_CREATION_PROGRESS"
const Prefix = "\x1eDEV_WORKSPACE_CREATION_PROGRESS/1 "
const MaxFrameBytes = 4096

type Event struct {
	Stage     string `json:"stage"`
	Event     string `json:"event"`
	ElapsedMs int64  `json:"elapsedMs"`
	Member    string `json:"member,omitempty"`
}

type Observer func(Event)

var memberPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,127}$`)
var stages = map[string]bool{
	"prepare": true, "conversation": true, "recovery_loaded": true,
	"recovery_index": true, "recovery_scan": true, "team_member": true,
	"prompt": true, "terminal": true, "evidence": true,
}

func (event Event) Valid() bool {
	return stages[event.Stage] && (event.Event == "begin" || event.Event == "finish") &&
		event.ElapsedMs >= 0 && event.ElapsedMs <= 86400000 &&
		(event.Event != "begin" || event.ElapsedMs == 0) &&
		(event.Member == "" || (event.Stage == "team_member" && memberPattern.MatchString(event.Member)))
}

// Begin returns a success-only completion callback. Durations belong to this
// stage and may overlap nested stages; they are never added across processes.
func Begin(observer Observer, stage, member string) func() {
	if observer == nil {
		return func() {}
	}
	started := time.Now()
	observer(Event{Stage: stage, Event: "begin", Member: member})
	return func() {
		observer(Event{Stage: stage, Event: "finish", Member: member, ElapsedMs: time.Since(started).Milliseconds()})
	}
}

func FromEnvironment() Observer {
	if os.Getenv(Environment) != "1" {
		return nil
	}
	return func(event Event) {
		if !event.Valid() {
			return
		}
		data, err := json.Marshal(event)
		if err == nil {
			_, _ = fmt.Fprintf(os.Stderr, "%s%s\n", Prefix, data)
		}
	}
}

func decode(data []byte) (Event, error) {
	if !utf8.Valid(data) {
		return Event{}, errors.New("invalid progress UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return Event{}, errors.New("invalid progress object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return Event{}, err
		}
		key, ok := token.(string)
		if !ok || (key != "stage" && key != "event" && key != "elapsedMs" && key != "member") || fields[key] != nil {
			return Event{}, errors.New("invalid progress field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return Event{}, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return Event{}, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Event{}, errors.New("trailing progress data")
	}
	for _, key := range []string{"stage", "event", "elapsedMs"} {
		if fields[key] == nil || bytes.Equal(fields[key], []byte("null")) {
			return Event{}, errors.New("missing progress field")
		}
	}
	var event Event
	if value, exists := fields["member"]; exists {
		var member string
		if json.Unmarshal(value, &member) != nil || !memberPattern.MatchString(member) {
			return Event{}, errors.New("invalid progress member")
		}
	}
	if err := json.Unmarshal(data, &event); err != nil || !event.Valid() {
		return Event{}, errors.New("invalid progress event")
	}
	return event, nil
}

// Decoder drains every byte, even after oversized or invalid candidate records.
// Ordinary diagnostics stream directly; candidate buffering never exceeds 4 KiB.
type Decoder struct {
	diagnostics   io.Writer
	observer      Observer
	pending       []byte
	discard       bool
	warned        bool
	rejectedBytes int
}

func (decoder *Decoder) reject(frame []byte) error {
	if decoder.rejectedBytes+len(frame) <= 16*1024 {
		decoder.rejectedBytes += len(frame)
		_, err := decoder.diagnostics.Write(frame)
		return err
	}
	if !decoder.warned {
		decoder.warned = true
		_, err := io.WriteString(decoder.diagnostics, "[invalid creation progress records omitted]\n")
		return err
	}
	return nil
}

func NewDecoder(diagnostics io.Writer, observer Observer) *Decoder {
	return &Decoder{diagnostics: diagnostics, observer: observer}
}

func (decoder *Decoder) Write(data []byte) (int, error) {
	for _, value := range data {
		if decoder.discard {
			if value == '\n' {
				decoder.discard = false
			}
			continue
		}
		if len(decoder.pending) == 0 && value != '\x1e' {
			if _, err := decoder.diagnostics.Write([]byte{value}); err != nil {
				return 0, err
			}
			continue
		}
		decoder.pending = append(decoder.pending, value)
		if len(decoder.pending) > MaxFrameBytes {
			decoder.pending = nil
			decoder.discard = value != '\n'
			if !decoder.warned {
				_, _ = io.WriteString(decoder.diagnostics, "[oversized creation progress record omitted]\n")
				decoder.warned = true
			}
			continue
		}
		if value == '\n' {
			frame := decoder.pending
			decoder.pending = nil
			if bytes.HasPrefix(frame, []byte(Prefix)) {
				if event, err := decode(frame[len(Prefix) : len(frame)-1]); err == nil {
					if decoder.observer != nil {
						decoder.observer(event)
					}
					continue
				}
			}
			if err := decoder.reject(frame); err != nil {
				return 0, err
			}
		}
	}
	return len(data), nil
}

func (decoder *Decoder) Close() error {
	err := decoder.reject(decoder.pending)
	decoder.pending = nil
	return err
}
