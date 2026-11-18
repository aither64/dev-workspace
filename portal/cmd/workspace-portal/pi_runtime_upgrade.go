package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func runPiRuntimeUpgrade(ctx context.Context, args []string, output io.Writer) error {
	values := make(map[string]string)
	if len(args) != 12 {
		return piruntime.ErrScope
	}
	for i := 0; i < len(args); i += 2 {
		key, value := args[i], args[i+1]
		switch key {
		case "--workspace", "--slug", "--request", "--expected-generation", "--socket", "--origin":
		default:
			return piruntime.ErrScope
		}
		if value == "" || values[key] != "" {
			return piruntime.ErrScope
		}
		values[key] = value
	}
	workspace, socket := values["--workspace"], values["--socket"]
	if !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || !session.ValidSlug(values["--slug"]) || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(values["--request"]) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(values["--expected-generation"]) {
		return piruntime.ErrScope
	}
	origin, err := url.Parse(values["--origin"])
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return piruntime.ErrScope
	}
	client, closeClient, err := privatePiPortalClient(socket)
	if err != nil {
		return err
	}
	defer closeClient()
	body, _ := json.Marshal(struct {
		Workspace          string `json:"workspace"`
		RequestID          string `json:"requestId"`
		ExpectedGeneration string `json:"expectedGeneration"`
	}{workspace, values["--request"], values["--expected-generation"]})
	target := "http://" + origin.Host + "/api/sessions/" + url.PathEscape(values["--slug"]) + "/pi-runtime-upgrade"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Origin", origin.String())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("Pi upgrade request %s is unconfirmed: %w", values["--request"], err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("Pi upgrade request %s returned HTTP %d; do not choose another request ID", values["--request"], response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return piruntime.ErrUnavailable
	}
	var result piruntime.UpgradeResult
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF || result.Runtime.Session.Workspace != workspace || result.Runtime.Session.Slug != values["--slug"] || result.Runtime.RequestID != values["--request"] || result.Previous.Generation != values["--expected-generation"] || (result.Phase != "ready" && result.Phase != "preparing" && result.Phase != "starting") {
		return piruntime.ErrScope
	}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return err
	}
	if result.Phase != "ready" {
		return fmt.Errorf("Pi upgrade request %s remains %s; reconcile this same request", values["--request"], result.Phase)
	}
	return nil
}

func privatePiPortalClient(socket string) (*http.Client, func(), error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return nil, nil, piruntime.ErrScope
	}
	info, err := os.Lstat(socket)
	if err != nil {
		return nil, nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0007 != 0 || owner.Uid != uint32(os.Geteuid()) {
		return nil, nil, piruntime.ErrScope
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &http.Client{Transport: transport, Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, transport.CloseIdleConnections, nil
}
