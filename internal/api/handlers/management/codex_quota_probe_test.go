package management

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexQuotaProbeIntervalIsHourly(t *testing.T) {
	t.Parallel()
	if codexQuotaProbeInterval != time.Hour {
		t.Fatalf("interval = %s, want 1h", codexQuotaProbeInterval)
	}
}

func TestProbeEnabledCodexWeeklyQuotas(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	enabled, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-enabled",
		Provider: "codex",
		Metadata: map[string]any{"access_token": "enabled-token", "account_id": "acct-enabled", "email": "enabled@example.com"},
	})
	if err != nil || enabled == nil {
		t.Fatalf("register enabled: %v", err)
	}
	disabled, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-disabled",
		Provider: "codex",
		Disabled: true,
		Status:   coreauth.StatusDisabled,
		Metadata: map[string]any{"access_token": "disabled-token", "email": "disabled@example.com"},
	})
	if err != nil || disabled == nil {
		t.Fatalf("register disabled: %v", err)
	}
	if _, err = manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-enabled",
		Provider: "claude",
		Metadata: map[string]any{"access_token": "claude-token"},
	}); err != nil {
		t.Fatalf("register claude: %v", err)
	}

	var calls atomic.Int32
	h := &Handler{
		authManager: manager,
		codexQuotaProbeDo: func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			if req.URL.String() != codexWhamUsageURL {
				t.Errorf("url = %s", req.URL)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer enabled-token" {
				t.Errorf("Authorization = %q", got)
			}
			if got := req.Header.Get("Chatgpt-Account-Id"); got != "acct-enabled" {
				t.Errorf("Chatgpt-Account-Id = %q", got)
			}
			if got := req.Header.Get("User-Agent"); got != codexQuotaProbeUserAgent {
				t.Errorf("User-Agent = %q", got)
			}
			body := `{"rate_limit":{"primary_window":{"used_percent":8,"limit_window_seconds":18000,"reset_at":1},"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":2}}}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		},
	}
	h.probeEnabledCodexWeeklyQuotas(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("probe calls = %d, want 1", calls.Load())
	}
	updated, ok := manager.GetByID(enabled.ID)
	if !ok || updated.Quota.Signals[coreauth.WeeklyQuotaRemainingPercentSignal] != "90%" {
		t.Fatalf("enabled signal = %#v", updated)
	}
	untouched, ok := manager.GetByID(disabled.ID)
	if !ok || len(untouched.Quota.Signals) != 0 {
		t.Fatalf("disabled auth was probed: %#v", untouched)
	}
}

func TestAPICallRecordsCodexWeeklyQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-manual",
		Provider: "codex",
		FileName: "/tmp/codex-manual.json",
		Metadata: map[string]any{"access_token": "manual-token", "email": "manual@example.com"},
	})
	if err != nil || auth == nil {
		t.Fatalf("register: %v", err)
	}
	auth.EnsureIndex()

	h := &Handler{
		authManager: manager,
		apiCallTransportForTest: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body := `{"rate_limit":{"secondary_window":{"used_percent":28,"limit_window_seconds":604800,"reset_at":2}}}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		}),
	}
	router := gin.New()
	router.POST("/api-call", h.APICall)

	if auth.Index == "" {
		t.Fatal("auth index is empty")
	}
	payload := `{"auth_index":"` + auth.Index + `","method":"GET","url":"https://chatgpt.com/backend-api/wham/usage","header":{"Authorization":"Bearer $TOKEN$"}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api-call", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated.Quota.Signals[coreauth.WeeklyQuotaRemainingPercentSignal] != "72%" {
		t.Fatalf("manual refresh signal = %#v", updated)
	}
}

func TestIsCodexWhamUsageURL(t *testing.T) {
	t.Parallel()
	parsed, err := url.Parse("https://chatgpt.com/backend-api/wham/usage?unused=1")
	if err != nil {
		t.Fatal(err)
	}
	if !isCodexWhamUsageURL(parsed) {
		t.Fatal("expected wham usage URL")
	}
	other, err := url.Parse("https://chatgpt.com/backend-api/subscriptions")
	if err != nil {
		t.Fatal(err)
	}
	if isCodexWhamUsageURL(other) {
		t.Fatal("subscriptions URL was treated as wham usage")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
