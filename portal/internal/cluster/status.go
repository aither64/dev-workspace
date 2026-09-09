package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/processgroup"
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

type Field struct {
	Label  string `json:"label"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
}

func (f *Field) UnmarshalJSON(data []byte) error {
	var decoded struct {
		Label  string `json:"label"`
		Value  string `json:"value"`
		Secret *bool  `json:"secret"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing field data")
	}
	if decoded.Secret == nil {
		return errors.New("missing secret flag")
	}
	*f = Field{Label: decoded.Label, Value: decoded.Value, Secret: *decoded.Secret}
	return nil
}

type Account struct {
	Label  string  `json:"label"`
	Fields []Field `json:"fields"`
}

type Service struct {
	Label    string    `json:"label"`
	URL      string    `json:"url,omitempty"`
	Accounts []Account `json:"accounts,omitempty"`
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
	Services    []Service    `json:"services,omitempty"`
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
	return r.InspectContext(context.Background(), slug)
}

// InspectContext reads all provider statuses while respecting the caller's
// deadline. Each provider also has its own five-second upper bound.
func (r Runner) InspectContext(ctx context.Context, slug string) ([]Status, error) {
	if !validSlug(slug) {
		return nil, errors.New("invalid session slug")
	}
	type result struct {
		status Status
		found  bool
		err    error
	}
	providers := r.providers()
	results := make([]result, len(providers))
	var wait sync.WaitGroup
	wait.Add(len(providers))
	for index, currentProvider := range providers {
		go func(index int, currentProvider provider) {
			defer wait.Done()
			results[index].status, results[index].found, results[index].err =
				r.inspectProvider(ctx, currentProvider, slug)
		}(index, currentProvider)
	}
	wait.Wait()
	var statuses []Status
	var problems []error
	for index, provider := range providers {
		result := results[index]
		if result.err != nil {
			problems = append(problems, fmt.Errorf("inspect %s cluster: %w", provider.name, result.err))
			continue
		}
		if result.found {
			statuses = append(statuses, result.status)
		}
	}
	return statuses, errors.Join(problems...)
}

// MayExist avoids launching provider helpers for sessions without cluster
// state. Unexpected filesystem errors and unsafe entries still return true so
// Inspect can validate and report them through the provider-owned contract.
func (r Runner) MayExist(slug string) bool {
	if !validSlug(slug) {
		return false
	}
	for _, provider := range r.providers() {
		path := filepath.Join(r.Workspace, ".dev-clusters", provider.name, "clusters", slug)
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
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

func (r Runner) inspectProvider(parent context.Context, provider provider, slug string) (Status, bool, error) {
	if provider.helper == "" {
		return Status{}, false, nil
	}
	if !filepath.IsAbs(provider.helper) {
		return Status{}, false, errors.New("development cluster helper is not absolute")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
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
	if (response.Schema != 1 && response.Schema != 2) || response.Kind != provider.name {
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
	if err := validateStructuredStatus(response.Schema, response.Status); err != nil {
		return Status{}, false, fmt.Errorf("development cluster helper returned an invalid status: %w", err)
	}
	if response.Schema == 1 {
		response.Services = legacyServices(response.Links, response.Credentials)
		response.Links = nil
		response.Credentials = nil
	}
	response.Label = provider.label
	return response.Status, true, nil
}

func validateStructuredStatus(schema int, status Status) error {
	if schema == 1 && status.Services != nil {
		return errors.New("schema 1 status contains services")
	}
	if schema == 2 && (status.Links != nil || status.Credentials != nil) {
		return errors.New("schema 2 status contains legacy links or credentials")
	}
	if schema == 2 && status.Services == nil {
		return errors.New("schema 2 status is missing services")
	}
	for _, command := range status.Commands {
		if command.Label == "" || command.Value == "" {
			return errors.New("command is incomplete")
		}
	}
	for _, link := range status.Links {
		if link.Label == "" || !safeServiceURL(link.URL) {
			return errors.New("link is invalid")
		}
	}
	for _, credential := range status.Credentials {
		if credential.Label == "" || credential.Value == "" {
			return errors.New("credential is incomplete")
		}
	}
	for _, service := range status.Services {
		if service.Label == "" ||
			(service.URL == "" && len(service.Accounts) == 0) ||
			(service.URL != "" && !safeServiceURL(service.URL)) {
			return errors.New("service is invalid")
		}
		for _, account := range service.Accounts {
			if account.Label == "" || len(account.Fields) == 0 {
				return errors.New("service account is incomplete")
			}
			for _, field := range account.Fields {
				if field.Label == "" || field.Value == "" {
					return errors.New("service account field is incomplete")
				}
			}
		}
	}
	return nil
}

func safeServiceURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") &&
		parsed.Host != "" && parsed.User == nil
}

func legacyServices(links []Link, credentials []Credential) []Service {
	services := make([]Service, 0, len(links)+1)
	for _, link := range links {
		services = append(services, Service{Label: link.Label, URL: link.URL})
	}
	if len(credentials) == 0 {
		return services
	}
	fields := make([]Field, 0, len(credentials))
	for _, credential := range credentials {
		fields = append(fields, Field{
			Label: credential.Label, Value: credential.Value,
			Secret: strings.Contains(strings.ToLower(credential.Label), "password"),
		})
	}
	services = append(services, Service{
		Label:    "Legacy credentials",
		Accounts: []Account{{Label: "Credentials", Fields: fields}},
	})
	return services
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
