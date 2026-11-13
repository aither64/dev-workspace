// Package pihosttools provides inert factories above the owning host services.
// No callback transport or worker registration is installed by these factories.
package pihosttools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type TeamHandlers interface {
	Report(context.Context, teamruntime.PiCaller, string, string) (teamruntime.PiDeliveryResult, error)
	Peer(context.Context, teamruntime.PiCaller, teamruntime.PiMessage) (teamruntime.PiDeliveryResult, error)
}

type TeamTools struct {
	caller   teamruntime.PiCaller
	handlers TeamHandlers
}

// BindTeamTools captures a caller from the trusted host channel, outside model
// arguments. The handler still revalidates it at the actual admission boundary.
func BindTeamTools(caller teamruntime.PiCaller, handlers TeamHandlers) (*TeamTools, error) {
	if err := teamruntime.ValidatePiCaller(caller); err != nil {
		return nil, err
	}
	return &TeamTools{caller: caller, handlers: handlers}, nil
}

func (tools *TeamTools) Call(ctx context.Context, name string, raw json.RawMessage) (teamruntime.PiDeliveryResult, error) {
	unavailable := teamruntime.PiDeliveryResult{Disposition: teamruntime.PiUnavailable}
	keys := []string{"message_id", "message"}
	if name == "team_message" {
		keys = append(keys, "to")
	} else if name != "report_to_lead" {
		return unavailable, teamruntime.ErrPiPolicy
	}
	args, err := teamArguments(raw, keys)
	if err != nil {
		return unavailable, err
	}
	to := args["to"]
	if name == "report_to_lead" {
		to = "lead"
	}
	message := teamruntime.PiMessage{ID: args["message_id"], To: to, Text: args["message"]}
	if teamruntime.ValidatePiMessage(message) != nil {
		return unavailable, teamruntime.ErrPiPolicy
	}
	if tools == nil || tools.handlers == nil {
		return unavailable, teamruntime.ErrPiUnavailable
	}
	if name == "report_to_lead" {
		return tools.handlers.Report(ctx, tools.caller, message.ID, message.Text)
	}
	return tools.handlers.Peer(ctx, tools.caller, message)
}

func teamArguments(raw []byte, keys []string) (map[string]string, error) {
	if len(raw) > 128*1024 || !utf8.Valid(raw) {
		return nil, teamruntime.ErrPiPolicy
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, teamruntime.ErrPiPolicy
	}
	args := make(map[string]string)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, teamruntime.ErrPiPolicy
		}
		allowed := false
		for _, field := range keys {
			allowed = allowed || key == field
		}
		if _, duplicate := args[key]; duplicate || !allowed {
			return nil, teamruntime.ErrPiPolicy
		}
		token, err = decoder.Token()
		value, ok := token.(string)
		if err != nil || !ok {
			return nil, teamruntime.ErrPiPolicy
		}
		args[key] = value
	}
	if last, err := decoder.Token(); err != nil || last != json.Delim('}') || len(args) != len(keys) {
		return nil, teamruntime.ErrPiPolicy
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, teamruntime.ErrPiPolicy
	}
	return args, nil
}
