package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

// Private transport only. Ruby owns outbox persistence and admission locks.
// It saves options before sending, and always retries the same native identity.
func coordinationNotify(args []string) error {
	if len(args) != 2 || args[0] != "--request" {
		return errors.New("coordination-notify requires --request FILE")
	}
	f, err := os.Open(args[1])
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 262144 {
		return errors.New("invalid private notification request")
	}
	var request struct {
		Schema        int                `json:"schema"`
		Action        string             `json:"action"`
		Workspace     string             `json:"workspace"`
		Slug          string             `json:"slug"`
		Socket        string             `json:"socket"`
		Thread        string             `json:"thread"`
		StateRoot     string             `json:"stateRoot"`
		Text          string             `json:"text"`
		MessageID     string             `json:"messageId"`
		ActionContext string             `json:"actionContext"`
		Options       *codex.TurnOptions `json:"options"`
	}
	d := json.NewDecoder(io.LimitReader(f, 262145))
	d.DisallowUnknownFields()
	if err = d.Decode(&request); err != nil {
		return err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing notification input")
	}
	if request.Schema != 1 || !session.ValidSlug(request.Slug) || request.Thread == "" {
		return errors.New("invalid notification identity")
	}
	summary, err := session.Find(request.Workspace, request.Slug)
	if err != nil {
		return err
	}
	if summary.Archived || summary.Codex.ThreadID != request.Thread || summary.Codex.SocketPath != request.Socket {
		return errors.New("notification owner changed or archived")
	}
	client := newCodexClient(request.Socket, request.Workspace, request.StateRoot)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	metadata, err := client.ReadThreadMetadata(ctx, request.Thread, true)
	if err != nil {
		return err
	}
	if metadata.ID != request.Thread || metadata.Cwd != filepath.Join(request.Workspace, "work", request.Slug) {
		return errors.New("native conversation identity differs")
	}
	if request.Action == "verify" {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": 1, "verified": true})
	}
	if request.Action == "prepare" {
		settings, err := client.ReadThreadSettings(ctx, request.Thread)
		if err != nil {
			return err
		}
		instructions := ""
		store, err := teamruntime.NewStore(request.StateRoot, request.Workspace)
		if err != nil {
			return err
		}
		roster, err := store.Load(request.Slug, request.Thread)
		if err == nil {
			instructions = roster.LeadInstructions
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		options := codex.TurnOptions{Model: settings.Model, ReasoningEffort: settings.ReasoningEffort,
			ThreadPolicy: workspacecodex.LeadThreadPolicy(request.Slug, request.Workspace, instructions)}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": 1, "options": options})
	}
	if request.Action != "send" || request.Options == nil || request.MessageID == "" || request.Text == "" || request.ActionContext == "" {
		return errors.New("invalid notification send")
	}
	receipt, found, err := client.ReconcileSendWithOptions(ctx, request.Thread, request.Text, request.MessageID, request.ActionContext, *request.Options)
	if err != nil {
		return err
	}
	if !found {
		receipt, err = client.SendWithOptions(ctx, request.Thread, request.Text, request.MessageID, request.ActionContext, *request.Options)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": 1, "receipt": receipt})
}
