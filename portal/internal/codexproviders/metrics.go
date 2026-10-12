package codexproviders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"time"
)

type Quota struct {
	Type      string     `json:"limit_type"`
	Window    string     `json:"limit_window"`
	Model     *string    `json:"model_filter,omitempty"`
	Maximum   *float64   `json:"max_value"`
	Used      *float64   `json:"current_value"`
	Remaining *float64   `json:"remaining_value"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
	Source    string     `json:"source"`
}

type Metrics struct {
	KeyQuotas      []Quota            `json:"keyQuotas"`
	UpstreamQuotas []Quota            `json:"upstreamQuotas"`
	PoolRemaining  map[string]float64 `json:"poolRemaining,omitempty"`
	Requests       *int64             `json:"requests,omitempty"`
	Tokens         *int64             `json:"tokens,omitempty"`
	CostUSD        *float64           `json:"costUSD,omitempty"`
}

func CredentialIdentity(p Provider) (string, error) {
	info, err := os.Stat(p.TokenFile)
	if err != nil {
		return "", errors.New("provider credential is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16384 {
		return "", errors.New("provider credential must be a private file")
	}
	data, err := readBounded(p.TokenFile, 16384, true)
	if err != nil {
		return "", errors.New("provider credential is unavailable")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("provider credential is invalid")
	}
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:]), nil
}

func ReadMetrics(ctx context.Context, p Provider) (Metrics, error) {
	if p.Kind != "codex-lb" {
		return Metrics{}, errors.New("provider does not report load balancer metrics")
	}
	data, err := providerGET(ctx, p, "/usage")
	if err != nil {
		return Metrics{}, err
	}
	var usage struct {
		Limits   []Quota             `json:"limits"`
		Upstream []Quota             `json:"upstream_limits"`
		Pool     map[string]*float64 `json:"account_pool_usage"`
		Requests *int64              `json:"request_count"`
		Tokens   *int64              `json:"total_tokens"`
		Cost     *float64            `json:"total_cost_usd"`
	}
	if json.Unmarshal(data, &usage) != nil {
		return Metrics{}, errors.New("invalid provider metrics response")
	}
	result := Metrics{KeyQuotas: []Quota{}, UpstreamQuotas: []Quota{}, PoolRemaining: map[string]float64{}, Requests: usage.Requests, Tokens: usage.Tokens, CostUSD: usage.Cost}
	for _, q := range usage.Limits {
		if !validQuota(q) {
			return Metrics{}, errors.New("invalid provider quota")
		}
		if q.Source == "aggregate" {
			if usage.Upstream == nil {
				result.UpstreamQuotas = append(result.UpstreamQuotas, q)
			}
		} else if q.Source == "api_key_limit" {
			result.KeyQuotas = append(result.KeyQuotas, q)
		} else {
			return Metrics{}, errors.New("unrecognized provider quota source")
		}
	}
	for _, q := range usage.Upstream {
		if !validQuota(q) {
			return Metrics{}, errors.New("invalid upstream quota")
		}
		result.UpstreamQuotas = append(result.UpstreamQuotas, q)
	}
	for _, key := range []string{"primary", "secondary"} {
		if value := usage.Pool[key]; value != nil && *value >= 0 && *value <= 100 && !math.IsNaN(*value) {
			result.PoolRemaining[key] = *value
		}
	}
	return result, nil
}

func validQuota(q Quota) bool {
	if q.Maximum == nil || q.Used == nil || q.Remaining == nil {
		return false
	}
	return q.Type != "" && len(q.Type) <= 128 && len(q.Window) <= 128 && len(q.Source) <= 128 && *q.Maximum >= 0 && *q.Used >= 0 && *q.Remaining >= 0 && !math.IsInf(*q.Maximum, 0) && (q.Model == nil || len(*q.Model) <= 512)
}
