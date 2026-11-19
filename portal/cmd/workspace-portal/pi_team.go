package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
)

type piTeamCommandOptions struct{ StateRoot, AuthorityDir, Workspace, Slug, Socket, Origin, Command, From, To, Message, MessageID, Role, Model, Effort, Address string }

// The serving portal remains the sole callback attachment and mutation owner.
// This finite client never acquires the command's runtime EX or creates a worker.
func runPiTeamCommand(ctx context.Context, options piTeamCommandOptions, output io.Writer) error {
	if options.Command != "list" && options.Command != "assign" && options.Command != "add" && options.Command != "remove" {
		return piruntime.ErrUnavailable
	}
	binding, err := (session.BackendBindingStore{Root: options.StateRoot, Workspace: options.Workspace}).Load(options.Slug)
	if err != nil || binding.Backend != agentbackend.Pi || binding.State != "ready" {
		return piruntime.ErrScope
	}
	authority, err := session.LoadPiRuntimeAuthority(options.AuthorityDir, options.Slug, options.Workspace)
	if err != nil || (options.Command != "add" && authority.ValidateBinding(binding) != nil) || authority.State != "ready" || authority.SessionIncarnation != binding.SessionIncarnation {
		return piruntime.ErrHeld
	}
	if options.From == "" {
		options.From = "lead"
	}
	caller, err := binding.Resolve(options.From)
	if err != nil {
		return err
	}
	origin, err := url.Parse(options.Origin)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return piruntime.ErrScope
	}
	method := http.MethodGet
	var body []byte
	if options.Command == "assign" {
		if teamruntime.ValidatePiMessage(teamruntime.PiMessage{ID: options.MessageID, To: options.To, Text: options.Message}) != nil {
			return piruntime.ErrScope
		}
		method = http.MethodPost
		body, err = json.Marshal(map[string]string{"action": "assign", "from": options.From, "to": options.To, "message": options.Message, "messageId": options.MessageID})
		if err != nil {
			return err
		}
	}
	if options.Command == "add" {
		if options.From != "lead" || len(options.MessageID) != 36 {
			return piruntime.ErrScope
		}
		method = http.MethodPost
		body, err = json.Marshal(map[string]string{"action": "add", "from": options.From, "role": options.Role, "model": options.Model, "reasoningEffort": options.Effort, "messageId": options.MessageID})
		if err != nil {
			return err
		}
	}
	if options.Command == "remove" {
		if options.From != "lead" || options.Address == "" || options.Address == "lead" {
			return piruntime.ErrScope
		}
		method = http.MethodDelete
		body, err = json.Marshal(map[string]string{"action": "remove", "address": options.Address})
		if err != nil {
			return err
		}
	}
	client, closeClient, err := privatePiPortalClient(options.Socket)
	if err != nil {
		return err
	}
	defer closeClient()
	request, err := http.NewRequestWithContext(ctx, method, "http://"+origin.Host+"/api/sessions/"+url.PathEscape(options.Slug)+"/team", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Origin", origin.String())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Workspace-Conversation-Scope", portalweb.PiConversationScope(caller))
	request.Header.Set("X-Workspace-Pi-Generation", authority.Generation)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("Pi team request %s is unconfirmed; retain the same message ID: %w", options.MessageID, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Pi team request %s returned HTTP %d; retain the same message ID", options.MessageID, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return piruntime.ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if options.Command == "remove" {
		var result struct {
			Roster teamruntime.PiRoster `json:"roster"`
		}
		if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Roster.Validate() != nil || result.Roster.Scope().Ref() != caller.Session {
			return piruntime.ErrScope
		}
		found := false
		for _, m := range result.Roster.Members {
			if m.Address == options.Address && m.State == "removed" {
				found = true
			}
		}
		if !found {
			return piruntime.ErrScope
		}
		return json.NewEncoder(output).Encode(result)
	}
	if options.Command == "add" {
		var result struct {
			Member  teamruntime.PiRosterMember `json:"member"`
			Roster  teamruntime.PiRoster       `json:"roster"`
			Backend string                     `json:"backend"`
		}
		if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Backend != "pi" || result.Roster.Scope().Ref() != caller.Session || result.Roster.Validate() != nil || result.Member.Addition == nil || result.Member.Addition.RequestID != options.MessageID {
			return piruntime.ErrScope
		}
		return json.NewEncoder(output).Encode(result)
	}
	if options.Command == "list" {
		var result struct {
			Roster      *teamruntime.PiRoster    `json:"roster"`
			DerivedSolo *portalweb.PiDerivedSolo `json:"derivedSolo,omitempty"`
			Backend     string                   `json:"backend"`
			Session     agentbackend.SessionRef  `json:"session"`
			Generation  string                   `json:"generation"`
		}
		if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Backend != "pi" || result.Session != caller.Session || result.Generation != authority.Generation {
			return piruntime.ErrScope
		}
		if result.Roster != nil {
			if result.DerivedSolo != nil || result.Roster.Validate() != nil || result.Roster.Scope().Ref() != caller.Session {
				return piruntime.ErrScope
			}
		} else if result.DerivedSolo == nil || len(binding.Conversations) != 1 || result.DerivedSolo.LeadModel == "" || result.DerivedSolo.LeadEffort == "" || result.DerivedSolo.Members == nil || len(result.DerivedSolo.Members) != 0 {
			return piruntime.ErrScope
		}
		return json.NewEncoder(output).Encode(result)
	}
	var result teamruntime.PiDeliveryResult
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Disposition != teamruntime.PiAttempted || (result.Outcome != backend.Accepted && result.Outcome != backend.Pending && result.Outcome != backend.Unknown && result.Outcome != backend.NotSubmitted) {
		return piruntime.ErrScope
	}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return err
	}
	if result.Outcome != backend.Accepted {
		return fmt.Errorf("Pi team request %s remains %s; enquire with the same message ID", options.MessageID, result.Outcome)
	}
	return nil
}
