package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	root := filepath.Join(r.Workspace, ".dev-clusters", kind)
	path := filepath.Join(root, "clusters", slug)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || filepath.Dir(path) != filepath.Join(root, "clusters") {
		return Status{}, false, errors.New("cluster state is not a real session directory")
	}
	status := Status{Kind: kind, Label: map[string]string{"vpsadmin": "vpsAdmin", "vpsadminos": "vpsAdminOS"}[kind]}
	status.Topology, _ = readSmallText(filepath.Join(path, "topology"))
	status.Network, _ = readSmallText(filepath.Join(path, "network"))
	status.Ready = regularFile(filepath.Join(path, "ready"))
	status.State = "stopped"
	if pidText, err := readSmallText(filepath.Join(path, "runner.pid")); err == nil {
		pid, parseErr := strconv.Atoi(pidText)
		if parseErr == nil && processMatches(pid, socketDir(kind, slug)) {
			status.State = "running"
		}
	}
	if status.Ready && status.State != "running" {
		status.State = "stale"
	}
	configPath := filepath.Join(path, "config.json")
	configInfo, err := os.Lstat(configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return status, true, nil
		}
		return Status{}, false, err
	}
	if configInfo.Mode()&os.ModeSymlink != 0 || !configInfo.Mode().IsRegular() || configInfo.Size() > 1024*1024 {
		return Status{}, false, errors.New("unsafe cluster config file")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return Status{}, false, err
	}
	if len(data) > 1024*1024 {
		return Status{}, false, errors.New("cluster config exceeds 1 MiB")
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return Status{}, false, fmt.Errorf("parse config: %w", err)
	}
	if kind == "vpsadmin" {
		populateVpsadmin(&status, slug, config)
	} else {
		populateVpsadminOS(&status, slug, config)
	}
	return status, true, nil
}

func populateVpsadmin(status *Status, slug string, config map[string]any) {
	domains, _ := config["domains"].(map[string]any)
	for _, item := range []struct{ key, label string }{
		{"webui", "Web UI"}, {"webCs", "Czech website"}, {"webEn", "English website"},
		{"api", "API"}, {"auth", "Authentication"}, {"console", "Console"},
		{"mailpit", "Mailpit"}, {"adminer", "Adminer"}, {"status", "Status"},
	} {
		domain, _ := domains[item.key].(string)
		if domain == "" {
			continue
		}
		port := ""
		if status.Network == "local" {
			port = ":10443"
		}
		status.Links = append(status.Links, Link{Label: item.label, URL: "https://" + domain + port + "/"})
	}
	status.Credentials = []Credential{
		{Label: "Admin login", Value: "test-admin"}, {Label: "Admin password", Value: "testAdminPassword"},
		{Label: "User login", Value: "test-user1"}, {Label: "User password", Value: "testUser1Password"},
		{Label: "Second user login", Value: "test-user2"}, {Label: "Second user password", Value: "testUser2Password"},
	}
	for _, item := range []struct {
		label string
		path  []string
	}{
		{"Adminer login", []string{"adminer", "webAuth", "username"}},
		{"Adminer password", []string{"adminer", "webAuth", "password"}},
		{"Mailpit login", []string{"mail", "capture", "webAuth", "username"}},
		{"Mailpit password", []string{"mail", "capture", "webAuth", "password"}},
	} {
		if value := nestedString(config, item.path...); value != "" {
			status.Credentials = append(status.Credentials, Credential{Label: item.label, Value: value})
		}
	}
	for _, machine := range topologyMembers(config, status.Topology, true) {
		status.Commands = append(status.Commands, Command{
			Label: machine, Value: "vpsadmin-devcluster ssh " + slug + " " + machine,
		})
	}
}

func nestedString(value map[string]any, path ...string) string {
	var current any = value
	for _, component := range path {
		mapping, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = mapping[component]
	}
	result, _ := current.(string)
	return result
}

func populateVpsadminOS(status *Status, slug string, config map[string]any) {
	for _, node := range topologyMembers(config, status.Topology, false) {
		status.Commands = append(status.Commands, Command{
			Label: node, Value: "vpsadminos-devcluster ssh " + slug + " " + node,
		})
	}
}

func topologyMembers(config map[string]any, topology string, includeServices bool) []string {
	var result []string
	if includeServices {
		result = append(result, "services")
	}
	topologies, _ := config["topologies"].(map[string]any)
	members, _ := topologies[topology].([]any)
	for _, member := range members {
		if name, ok := member.(string); ok && validSlug(name) {
			result = append(result, name)
		}
	}
	return result
}

func readSmallText(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 4096 {
		return "", errors.New("unsafe cluster state file")
	}
	data, err := os.ReadFile(path)
	return strings.TrimSpace(string(data)), err
}

func regularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

func processMatches(pid int, socket string) bool {
	if pid <= 1 {
		return false
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	for _, argument := range strings.Split(string(data), "\x00") {
		if argument == socket {
			return true
		}
	}
	return false
}

func socketDir(kind, slug string) string {
	prefix := map[string]string{"vpsadmin": "vpsfree-devcluster-", "vpsadminos": "vpsadminos-devcluster-"}[kind]
	digest := sha256.Sum256([]byte(slug))
	return filepath.Join("/tmp", prefix+hex.EncodeToString(digest[:])[:12])
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
