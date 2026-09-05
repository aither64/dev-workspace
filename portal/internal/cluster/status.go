package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type Command struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type Credential struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type Status struct {
	Kind        string       `json:"kind"`
	Label       string       `json:"label"`
	State       string       `json:"state"`
	Ready       bool         `json:"ready"`
	Topology    string       `json:"topology,omitempty"`
	Network     string       `json:"network,omitempty"`
	Links       []Link       `json:"links,omitempty"`
	Commands    []Command    `json:"commands,omitempty"`
	Credentials []Credential `json:"credentials,omitempty"`
}

type Runner struct {
	Workspace  string
	Vpsadmin   string
	VpsadminOS string
}

func (r Runner) Inspect(slug string) ([]Status, error) {
	if !validSlug(slug) {
		return nil, errors.New("invalid session slug")
	}
	var statuses []Status
	var problems []error
	for _, kind := range []string{"vpsadmin", "vpsadminos"} {
		status, found, err := r.inspectKind(kind, slug)
		if err != nil {
			problems = append(problems, fmt.Errorf("inspect %s cluster: %w", kind, err))
			continue
		}
		if found {
			statuses = append(statuses, status)
		}
	}
	return statuses, errors.Join(problems...)
}

func (r Runner) Release(ctx context.Context, kind, slug string) error {
	if !validSlug(slug) {
		return errors.New("invalid session slug")
	}
	var helper string
	switch kind {
	case "vpsadmin":
		helper = r.Vpsadmin
	case "vpsadminos":
		helper = r.VpsadminOS
	default:
		return errors.New("unknown development cluster")
	}
	if helper == "" || !filepath.IsAbs(helper) {
		return errors.New("development cluster helper is unavailable")
	}
	command := exec.CommandContext(ctx, helper, "reset", slug)
	command.Env = append(os.Environ(), "VPSFREE_DEVCLUSTER_WORKSPACE="+r.Workspace)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return errors.New(message)
	}
	return nil
}

func (r Runner) inspectKind(kind, slug string) (Status, bool, error) {
	helper := r.Vpsadmin
	if kind == "vpsadminos" {
		helper = r.VpsadminOS
	}
	if helper == "" {
		return Status{}, false, nil
	}
	if !filepath.IsAbs(helper) {
		return Status{}, false, errors.New("development cluster helper is not absolute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, helper, "status", slug, "--json")
	command.Env = append(os.Environ(), "VPSFREE_DEVCLUSTER_WORKSPACE="+r.Workspace)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return Status{}, false, errors.New(message)
	}
	var response struct {
		Schema int  `json:"schema"`
		Found  bool `json:"found"`
		Status
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return Status{}, false, fmt.Errorf("decode helper status: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Status{}, false, errors.New("development cluster helper returned trailing output")
	}
	if response.Schema != 1 || response.Kind != kind {
		return Status{}, false, errors.New("development cluster helper returned an incompatible status")
	}
	if !response.Found {
		return Status{}, false, nil
	}
	if response.Label == "" || (response.State != "running" && response.State != "stopped" && response.State != "stale") {
		return Status{}, false, errors.New("development cluster helper returned an invalid status")
	}
	return response.Status, true, nil
}

func validSlug(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || (index > 0 && (character == '_' || character == '-')) {
			continue
		}
		return false
	}
	return true
}
