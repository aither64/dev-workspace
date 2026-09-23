package web

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

var memberConversationAddress = regexp.MustCompile(`^[a-z][a-z0-9]{0,63}$`)

func teamConversationID(slug, address string) string { return slug + "~" + address }

func parseTeamConversationID(id string) (slug, address string, valid bool) {
	slug, address, member := strings.Cut(id, "~")
	if !session.ValidSlug(slug) || (member && !memberConversationAddress.MatchString(address)) {
		return "", "", false
	}
	if !member {
		return slug, "", true
	}
	return slug, address, true
}

func readyConversationMember(store *teamruntime.Store, slug, rootThreadID, address string) (teamruntime.Member, error) {
	roster, err := store.Load(slug, rootThreadID)
	if err != nil {
		return teamruntime.Member{}, err
	}
	for _, member := range roster.Members {
		if member.Address == address && member.State == "ready" && member.RetireIntent == "" && member.Thread != "" {
			return member, nil
		}
	}
	return teamruntime.Member{}, errors.New("team member is unavailable")
}

// memberConversationClient keeps direct browser turns on the roster's policy.
// Embedding preserves the normal conversation features and retry receipts.
type memberConversationClient struct {
	conversation.Client
	server       *Server
	service      teamruntime.Service
	slug         string
	rootThreadID string
	address      string
	threadID     string
}

func (client memberConversationClient) member() (teamruntime.Member, error) {
	member, err := readyConversationMember(client.service.Store, client.slug, client.rootThreadID, client.address)
	if err != nil || member.Thread != client.threadID {
		return teamruntime.Member{}, errors.New("team member is unavailable")
	}
	return member, nil
}

func (client memberConversationClient) ReadThread(ctx context.Context, threadID string) (codex.Transcript, error) {
	member, err := client.member()
	if err != nil || threadID != client.threadID {
		return codex.Transcript{}, errors.New("team member is unavailable")
	}
	transcript, err := client.Client.ReadThread(ctx, threadID)
	if err == nil {
		transcript.Model, transcript.ReasoningEffort = member.Model, member.Effort
	}
	return transcript, err
}

func (client memberConversationClient) Send(ctx context.Context, threadID, message, clientID, actionContext string) (codex.SendReceipt, error) {
	if threadID != client.threadID {
		return codex.SendReceipt{}, errors.New("team member is unavailable")
	}
	options, err := client.turnOptions(message, clientID, actionContext)
	if err != nil {
		return codex.SendReceipt{}, err
	}
	return client.service.Client.SendWithOptions(ctx, threadID, message, clientID, actionContext,
		options)
}

func (client memberConversationClient) SendAttempted(ctx context.Context, threadID, message, clientID, actionContext string) (bool, error) {
	if threadID != client.threadID {
		return false, errors.New("team member is unavailable")
	}
	discovery, ok := client.Client.(interface {
		SendAttemptedWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (bool, error)
	})
	if !ok {
		return false, errors.New("member send recovery is unavailable")
	}
	options, err := client.turnOptions(message, clientID, actionContext)
	if err != nil {
		return false, err
	}
	return discovery.SendAttemptedWithOptions(ctx, threadID, message, clientID, actionContext, options)
}

func (client memberConversationClient) turnOptions(message, clientID, actionContext string) (codex.TurnOptions, error) {
	member, err := client.member()
	if err != nil {
		return codex.TurnOptions{}, err
	}
	policy, err := client.service.MemberTurnPolicy(client.slug, client.rootThreadID, member)
	if err != nil {
		return codex.TurnOptions{}, err
	}
	current := codex.TurnOptions{Model: member.Model, ReasoningEffort: member.Effort, ThreadPolicy: policy}
	discovery, ok := client.Client.(interface {
		OriginalSendOptions(string, string, string, string) (codex.TurnOptions, bool, error)
	})
	if !ok {
		return codex.TurnOptions{}, errors.New("member send recovery is unavailable")
	}
	original, found, err := discovery.OriginalSendOptions(client.threadID, message, clientID, actionContext)
	if err != nil {
		return codex.TurnOptions{}, err
	}
	if !found {
		return current, nil
	}
	if !sameThreadPolicy(original.ThreadPolicy, policy) {
		return codex.TurnOptions{}, errors.New("member policy changed since this message was attempted")
	}
	return original, nil
}

func sameThreadPolicy(left, right codex.ThreadPolicy) bool {
	if left.DeveloperInstructions != right.DeveloperInstructions || left.Sandbox != right.Sandbox {
		return false
	}
	if left.MCPServer == nil || right.MCPServer == nil {
		return left.MCPServer == nil && right.MCPServer == nil
	}
	return left.MCPServer.Name == right.MCPServer.Name && left.MCPServer.Command == right.MCPServer.Command &&
		left.MCPServer.Tool == right.MCPServer.Tool && slices.Equal(left.MCPServer.Args, right.MCPServer.Args)
}

func (client memberConversationClient) RespondPrompt(ctx context.Context, threadID string, response codex.PromptResponse) error {
	responder, ok := client.Client.(conversation.PromptResponder)
	if !ok {
		return errors.New("member prompt response is unavailable")
	}
	return responder.RespondPrompt(ctx, threadID, response)
}

func (client memberConversationClient) DeleteQueueEntryWithCompletion(ctx context.Context, threadID, id string, complete func() error) error {
	queue, ok := client.Client.(conversation.QueueDeletionCompleter)
	if !ok {
		return errors.New("member queue deletion recovery is unavailable")
	}
	return queue.DeleteQueueEntryWithCompletion(ctx, threadID, id, complete)
}

func (client memberConversationClient) ReconcileQueueDeletionsWithCompletion(ctx context.Context, threadID string, complete func(string) error) error {
	queue, ok := client.Client.(conversation.QueueDeletionCompleter)
	if !ok {
		return errors.New("member queue deletion recovery is unavailable")
	}
	return queue.ReconcileQueueDeletionsWithCompletion(ctx, threadID, complete)
}

func (client memberConversationClient) StartQueue(ctx context.Context, threadID, queuedID string) error {
	member, err := client.member()
	if err != nil || threadID != client.threadID {
		return errors.New("team member is unavailable")
	}
	starter, ok := client.Client.(interface {
		StartQueueWithPolicy(context.Context, string, string, codex.ThreadPolicy) error
	})
	if !ok {
		return errors.New("member queue startup is unavailable")
	}
	policy, err := client.service.MemberTurnPolicy(client.slug, client.rootThreadID, member)
	if err != nil {
		return err
	}
	_, err = client.Client.UpdateThreadSettings(ctx, threadID, codex.ThreadSettingsUpdate{
		Model: &member.Model, ReasoningEffort: &member.Effort,
	})
	if err != nil {
		return err
	}
	return starter.StartQueueWithPolicy(ctx, threadID, queuedID, policy)
}

func (client memberConversationClient) UpdateThreadSettings(ctx context.Context, threadID string, update codex.ThreadSettingsUpdate) (codex.ThreadSettings, error) {
	member, err := client.member()
	if err != nil || threadID != client.threadID {
		return codex.ThreadSettings{}, errors.New("team member is unavailable")
	}
	model, effort := member.Model, member.Effort
	if update.Model != nil {
		model = *update.Model
	}
	if update.ReasoningEffort != nil {
		effort = *update.ReasoningEffort
	}
	if err := client.server.validateModelSettings(ctx, codex.ThreadSettings{Model: model, ReasoningEffort: effort}, false); err != nil {
		return codex.ThreadSettings{}, err
	}
	settings, err := client.Client.UpdateThreadSettings(ctx, threadID, update)
	if err != nil {
		return codex.ThreadSettings{}, err
	}
	if model != member.Model || effort != member.Effort {
		_, err = client.service.Store.Update(ctx, client.slug, client.rootThreadID, false, func(roster *teamruntime.Roster) error {
			for index := range roster.Members {
				current := &roster.Members[index]
				if current.Address == client.address && current.State == "ready" && current.Thread == threadID && current.RetireIntent == "" {
					current.Model, current.Effort = model, effort
					return nil
				}
			}
			return errors.New("team member is unavailable")
		})
		if err != nil {
			return codex.ThreadSettings{}, err
		}
	}
	settings.Model, settings.ReasoningEffort = model, effort
	return settings, nil
}
