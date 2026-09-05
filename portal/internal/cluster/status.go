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

	"github.com/aither64/vpsfree-cz-workspace/portal/internal/processgroup"
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

type provider struct {
	name   string
	label  string
	helper string
}

func (r Runner) providers() []provider {
	return []provider{
		{name: "vpsadmin", label: "vpsAdmin", helper: r.Vpsadmin},
		{name: "vpsadminos", label: "vpsAdminOS", helper: r.VpsadminOS},
	}
}

func (r Runner) Inspect(slug string) ([]Status, error) {
	if !validSlug(slug) {
		return nil, errors.New("invalid session slug")
	}
	var statuses []Status
	var problems []error
	for _, provider := range r.providers() {
		status, found, err := r.inspectProvider(provider, slug)
		if err != nil {
			problems = append(problems, fmt.Errorf("inspect %s cluster: %w", provider.name, err))
			continue
		}
		if found {
			statuses = append(statuses, status)
		}
	}
	return statuses, errors.Join(problems...)
}

// ReleaseAll invokes every provider unconditionally. Each helper serializes
// reset with starts for the same session, so this is also the archive barrier:
// a cluster cannot appear between a one-time status snapshot and finalization.
func (r Runner) ReleaseAll(ctx context.Context, slug string) error {
	var problems []error
	for _, provider := range r.providers() {
		if err := r.releaseProvider(ctx, provider, slug); err != nil {
			problems = append(problems, fmt.Errorf("release %s cluster: %w", provider.label, err))
		}
	}
	return errors.Join(problems...)
}

func (r Runner) Release(ctx context.Context, kind, slug string) error {
	if !validSlug(slug) {
		return errors.New("invalid session slug")
	}
	for _, provider := range r.providers() {
		if provider.name == kind {
			return r.releaseProvider(ctx, provider, slug)
		}
	}
	return errors.New("unknown development cluster")
}

func (r Runner) releaseProvider(ctx context.Context, provider provider, slug string) error {
	if provider.helper == "" || !filepath.IsAbs(provider.helper) {
		return errors.New("development cluster helper is unavailable")
	}
	command := exec.Command(provider.helper, "reset", slug)
	command.Env = append(os.Environ(), "VPSFREE_DEVCLUSTER_WORKSPACE="+r.Workspace)
	output, err := processgroup.CombinedOutput(ctx, command)
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return errors.New(message)
	}
	return nil
}

func (r Runner) inspectProvider(provider provider, slug string) (Status, bool, error) {
	if provider.helper == "" {
		return Status{}, false, nil
	}
	if !filepath.IsAbs(provider.helper) {
		return Status{}, false, errors.New("development cluster helper is not absolute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.Command(provider.helper, "status", slug, "--json")
	command.Env = append(os.Environ(), "VPSFREE_DEVCLUSTER_WORKSPACE="+r.Workspace)
	output, err := processgroup.CombinedOutput(ctx, command)
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
	if response.Schema != 1 || response.Kind != provider.name {
		return Status{}, false, errors.New("development cluster helper returned an incompatible status")
	}
	if response.Label != "" {
		return Status{}, false, errors.New("development cluster helper returned an invalid status")
	}
	if !response.Found {
		return Status{}, false, nil
	}
	if response.State != "running" && response.State != "stopped" && response.State != "stale" {
		return Status{}, false, errors.New("development cluster helper returned an invalid status")
	}
	response.Label = provider.label
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
