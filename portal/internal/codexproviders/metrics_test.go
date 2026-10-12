package codexproviders

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testToken(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("private-test-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMetricsSeparateKeyQuotaAndDeduplicateUpstream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" || r.Header.Get("Authorization") != "Bearer private-test-token" {
			t.Error("invalid server-side metrics request")
		}
		_, _ = w.Write([]byte(`{"limits":[{"limit_type":"tokens","limit_window":"1d","source":"api_key_limit","max_value":100,"current_value":25,"remaining_value":75},{"limit_type":"credits","limit_window":"5h","source":"aggregate","max_value":500,"current_value":50,"remaining_value":450}],"upstream_limits":[{"limit_type":"credits","limit_window":"5h","source":"aggregate","max_value":500,"current_value":50,"remaining_value":450}],"account_pool_usage":{"primary":90,"secondary":87.5}}`))
	}))
	defer server.Close()
	metrics, err := ReadMetrics(context.Background(), Provider{Kind: "codex-lb", BaseURL: server.URL + "/v1", TokenFile: testToken(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics.KeyQuotas) != 1 || len(metrics.UpstreamQuotas) != 1 || *metrics.KeyQuotas[0].Remaining != 75 || metrics.PoolRemaining["secondary"] != 87.5 {
		t.Fatalf("metrics = %#v", metrics)
	}
	data, _ := json.Marshal(metrics)
	if strings.Contains(string(data), "private-test-token") || strings.Contains(string(data), server.URL) {
		t.Fatal("metrics exposed provider authentication or endpoint")
	}
}

func TestMetricsRefuseRedirectAndInvalidMissingQuota(t *testing.T) {
	for _, body := range []string{"redirect", `{"limits":[{"limit_type":"credits","limit_window":"5h","source":"aggregate"}]}`} {
		t.Run(body, func(t *testing.T) {
			redirected := false
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if body == "redirect" {
					http.Redirect(w, r, target.URL, 302)
				} else {
					_, _ = w.Write([]byte(body))
				}
			}))
			defer server.Close()
			_, err := ReadMetrics(context.Background(), Provider{Kind: "codex-lb", BaseURL: server.URL + "/v1", TokenFile: testToken(t)})
			if err == nil || redirected {
				t.Fatalf("err=%v redirected=%v", err, redirected)
			}
		})
	}
}

func TestCredentialRotationChangesCacheIdentity(t *testing.T) {
	path := testToken(t)
	p := Provider{TokenFile: path}
	first, err := CredentialIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if err := os.WriteFile(path, []byte("rotated-test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	second, err := CredentialIdentity(p)
	if err != nil || first == second {
		t.Fatalf("credential identity did not change: %v", err)
	}
}
