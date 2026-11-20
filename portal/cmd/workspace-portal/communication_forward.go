package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/session"
	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
)

// Administrative forwarding retains the source lead's identity and parent
// invocation. A Unix socket/scope header is never native tool-origin proof.
func forwardCommunication(ctx context.Context, workspace, stateRoot, authorityDir, socket, originText string, caller communication.EndpointRecord, request communication.Request, admission *communicationAdmission, output io.Writer) error {
	if admission == nil {
		return errors.New("communication parent invocation is required")
	}
	origin, err := url.Parse(originText)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return errors.New("communication portal origin is invalid")
	}
	client, closeClient, err := privatePiPortalClient(socket)
	if err != nil {
		return err
	}
	defer closeClient()
	scope, generation := "", ""
	if caller.Pi != nil {
		binding, err := (session.BackendBindingStore{Root: stateRoot, Workspace: workspace}).Load(caller.Slug())
		if err != nil {
			return err
		}
		authority, err := session.LoadPiRuntimeAuthority(authorityDir, caller.Slug(), workspace)
		if err != nil || authority.State != "ready" || authority.ValidateBinding(binding) != nil {
			return errors.New("communication source generation is unavailable")
		}
		lead, err := binding.Resolve("lead")
		if err != nil || lead != caller.Conversation() {
			return errors.New("communication source lead changed")
		}
		scope, generation = portalweb.PiConversationScope(lead), authority.Generation
	} else {
		scope = caller.Codex.Incarnation
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	forward, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+origin.Host+"/api/sessions/"+url.PathEscape(caller.Slug())+"/communication", bytes.NewReader(data))
	if err != nil {
		return err
	}
	forward.Header.Set("Origin", origin.String())
	forward.Header.Set("Content-Type", "application/json")
	forward.Header.Set("Accept", "application/json")
	forward.Header.Set("X-Workspace-Conversation-Scope", scope)
	if generation != "" {
		forward.Header.Set("X-Workspace-Pi-Generation", generation)
	}
	if err := admission.recheck(); err != nil {
		return err
	}
	response, err := client.Do(forward)
	if err != nil {
		return fmt.Errorf("communication request %s is unconfirmed; retain the original ID: %w", request.MessageID, err)
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("communication receipt exceeds its limit or is unavailable")
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("communication request %s returned HTTP %d; retain the original ID", request.MessageID, response.StatusCode)
	}
	var view communication.EndpointView
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&view) != nil || decoder.Decode(new(any)) != io.EOF || view.ValidatePublic() != nil {
		return errors.New("communication receipt could not be verified")
	}
	for _, grant := range view.Grants {
		if !sameCommunicationEndpoint(grant.A, caller) && !sameCommunicationEndpoint(grant.B, caller) {
			return errors.New("communication receipt has a foreign source")
		}
	}
	for _, message := range view.Messages {
		if !sameCommunicationEndpoint(message.From, caller) && !sameCommunicationEndpoint(message.To, caller) {
			return errors.New("communication receipt has a foreign source")
		}
	}
	if len(view.Messages) > 10 {
		view.Messages = view.Messages[len(view.Messages)-10:]
	}
	return json.NewEncoder(output).Encode(view)
}

func sameCommunicationEndpoint(a, b communication.EndpointRecord) bool {
	encodedA, _ := json.Marshal(a)
	encodedB, _ := json.Marshal(b)
	return bytes.Equal(encodedA, encodedB)
}
