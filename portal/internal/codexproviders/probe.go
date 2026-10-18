package codexproviders

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

func ProbeModels(ctx context.Context, p Provider, retained []string) error {
	data, err := providerGET(ctx, p, "/models")
	if err != nil {
		return &ProbeError{Code: "target_model_catalog_unavailable", Cause: err}
	}
	var response struct {
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &response) != nil || response.Data == nil {
		return &ProbeError{Code: "target_model_catalog_invalid"}
	}
	models := map[string]bool{}
	for _, model := range *response.Data {
		models[model.ID] = true
	}
	for _, model := range retained {
		if model == "" || !models[model] {
			return &ProbeError{Code: "target_model_unavailable", Model: model}
		}
	}
	return nil
}

// ProbeError exposes only a stable reason and the operator-selected model.
// Credential contents and HTTP endpoint details remain private.
type ProbeError struct {
	Code, Model string
	Cause       error
}

func (e *ProbeError) Error() string { return e.Code }
func (e *ProbeError) Unwrap() error { return e.Cause }

func providerGET(ctx context.Context, p Provider, path string) ([]byte, error) {
	if _, err := CredentialIdentity(p); err != nil {
		return nil, err
	}
	data, err := readBounded(p.TokenFile, 16384, true)
	if err != nil {
		return nil, errors.New("provider credential is unavailable")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("provider credential is invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+path, nil)
	if err != nil {
		return nil, errors.New("provider endpoint is invalid")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("provider endpoint is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("provider endpoint is unavailable")
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, errors.New("provider response exceeds bounds")
	}
	return data, nil
}
