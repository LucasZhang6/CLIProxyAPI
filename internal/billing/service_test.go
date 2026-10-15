package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestBillingLegacyTokenPricingAndSummary(t *testing.T) {
	svc := NewService(Config{Enabled: true, Mode: "observe", LedgerPath: t.TempDir() + "/billing.jsonl", Token: TokenConfig{LegacyAPIKeys: "observe"}, PriceBook: PriceBookConfig{Version: "test", Rules: []PriceRule{{ID: "rule", BillingProvider: "openai", Model: "gpt-5.5", InputUncachedNanosPerToken: 5000, InputCacheReadNanosPerToken: 500, OutputTextNanosPerToken: 30000, OutputReasoningNanosPerToken: 30000}}}}, []string{"sk-test"})
	p, ok := svc.AuthenticateToken("sk-test")
	if !ok || p.UserID != "legacy" || p.TokenID == "" {
		t.Fatalf("legacy principal = %+v ok=%v", p, ok)
	}
	ctx := WithPrincipal(context.Background(), p)
	svc.HandleUsage(ctx, coreusage.Record{Provider: "openai", Model: "gpt-5.5", RequestedAt: time.Now(), Detail: coreusage.Detail{InputTokens: 100000, CacheReadTokens: 80000, OutputTokens: 5000, TotalTokens: 105000}})
	summary := svc.Summary()
	if summary.Requests != 1 || summary.PricedRequests != 1 {
		t.Fatalf("summary counts = %+v", summary)
	}
	// 20k uncached*$5/1M + 80k cached*$0.5/1M + 5k output*$30/1M = $0.29 = 290,000,000 nanos.
	if summary.CustomerCostNanos != 290000000 {
		t.Fatalf("cost nanos = %d, want 290000000", summary.CustomerCostNanos)
	}
}

func TestBillingGuardRejectsUnknownModelWhenPolicyReject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := NewService(Config{Enabled: true, Mode: "enforce-hard", UnknownPricePolicy: "reject", LedgerPath: t.TempDir() + "/billing.jsonl", Token: TokenConfig{LegacyAPIKeys: "observe"}, PriceBook: PriceBookConfig{Version: "test", Rules: []PriceRule{{ID: "rule", BillingProvider: "openai", Model: "gpt-5.5", InputUncachedNanosPerToken: 1, OutputTextNanosPerToken: 1}}}}, []string{"sk-test"})
	r := gin.New()
	r.Use(svc.GuardMiddleware())
	r.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"future-model","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusPaymentRequired, rec.Body.String())
	}
}

func TestBillingEventsAfterCursor(t *testing.T) {
	svc := NewService(Config{Enabled: true, Mode: "observe", LedgerPath: t.TempDir() + "/billing.jsonl", Token: TokenConfig{LegacyAPIKeys: "observe"}, PriceBook: PriceBookConfig{Version: "test", Rules: []PriceRule{{ID: "rule", BillingProvider: "openai", Model: "gpt-5.5", InputUncachedNanosPerToken: 1, OutputTextNanosPerToken: 1}}}}, []string{"sk-test"})
	p, ok := svc.AuthenticateToken("sk-test")
	if !ok {
		t.Fatal("legacy token did not authenticate")
	}
	base := time.Now().Add(-time.Hour)
	svc.HandleUsage(WithPrincipal(context.Background(), p), coreusage.Record{RequestID: "one", Provider: "openai", Model: "gpt-5.5", RequestedAt: base, Detail: coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
	svc.HandleUsage(WithPrincipal(context.Background(), p), coreusage.Record{RequestID: "two", Provider: "openai", Model: "gpt-5.5", RequestedAt: base.Add(time.Second), Detail: coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
	all := svc.EventsAfter(time.Time{}, "", 0)
	if len(all) != 2 {
		t.Fatalf("EventsAfter initial len=%d, want 2", len(all))
	}
	next := svc.EventsAfter(all[0].CompletedAt, all[0].EventID, 10)
	if len(next) != 1 || next[0].RequestID != "two" {
		t.Fatalf("EventsAfter cursor = %+v, want only request two", next)
	}
	limited := svc.EventsAfter(time.Time{}, "", 1)
	if len(limited) != 1 || limited[0].RequestID != "one" {
		t.Fatalf("EventsAfter limit = %+v, want first request", limited)
	}
}

func TestBillingQuotaSurvivesServiceRestartFromLedger(t *testing.T) {
	ledger := t.TempDir() + "/billing.jsonl"
	cfg := Config{Enabled: true, Mode: "observe", LedgerPath: ledger, Token: TokenConfig{LegacyAPIKeys: "observe"}, Quota: QuotaConfig{DailyNanos: 1}, PriceBook: PriceBookConfig{Version: "test", Rules: []PriceRule{{ID: "rule", BillingProvider: "openai", Model: "gpt-5.5", InputUncachedNanosPerToken: 5000, OutputTextNanosPerToken: 30000}}}}
	svc := NewService(cfg, []string{"sk-test"})
	p, ok := svc.AuthenticateToken("sk-test")
	if !ok {
		t.Fatal("legacy token did not authenticate")
	}
	svc.HandleUsage(WithPrincipal(context.Background(), p), coreusage.Record{Provider: "openai", Model: "gpt-5.5", RequestedAt: time.Now(), Detail: coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})

	cfg.Mode = "enforce-soft"
	restarted := NewService(cfg, []string{"sk-test"})
	if _, err := restarted.acquire(p); err == nil {
		t.Fatal("expected persisted daily quota to reject after restart")
	}
}

func failoverBillingConfig(ledger string) Config {
	return Config{Enabled: true, Mode: "observe", LedgerPath: ledger, Token: TokenConfig{LegacyAPIKeys: "observe"}, PriceBook: PriceBookConfig{Version: "test", Rules: []PriceRule{{ID: "rule", BillingProvider: "openai", Model: "gpt-5.5", InputUncachedNanosPerToken: 10, OutputTextNanosPerToken: 100}}}}
}

func TestBillingSuccessfulFailoverAttemptSupersedesFailedAttempt(t *testing.T) {
	ledger := t.TempDir() + "/billing.jsonl"
	cfg := failoverBillingConfig(ledger)
	svc := NewService(cfg, []string{"sk-test"})
	p, ok := svc.AuthenticateToken("sk-test")
	if !ok {
		t.Fatal("legacy token did not authenticate")
	}
	ctx := WithPrincipal(context.Background(), p)
	svc.HandleUsage(ctx, coreusage.Record{RequestID: "req", Provider: "openai", Model: "gpt-5.5", AuthIndex: "overloaded", Failed: true, RequestedAt: time.Now()})
	svc.HandleUsage(ctx, coreusage.Record{RequestID: "req", Provider: "openai", Model: "gpt-5.5", AuthIndex: "rescuer", RequestedAt: time.Now(), Detail: coreusage.Detail{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}})

	const wantCost = int64(100*10 + 10*100)
	assertBilledOnce := func(t *testing.T, svc *Service) {
		t.Helper()
		summary := svc.Summary()
		if summary.Requests != 1 || summary.CustomerCostNanos != wantCost {
			t.Fatalf("summary requests=%d cost=%d, want 1 request costing %d", summary.Requests, summary.CustomerCostNanos, wantCost)
		}
		event, ok := svc.EventByRequestID("req")
		if !ok || event.Failed || event.AuthIndex != "rescuer" {
			t.Fatalf("event = %+v ok=%v, want successful rescuer attempt", event, ok)
		}
		if spend := svc.spendDay[p.UserID]; spend != wantCost {
			t.Fatalf("daily spend = %d, want %d", spend, wantCost)
		}
	}
	assertBilledOnce(t, svc)

	exported := svc.EventsAfter(time.Time{}, "", 0)
	if len(exported) != 1 || exported[0].Failed {
		t.Fatalf("exported events = %+v, want only the successful attempt", exported)
	}

	assertBilledOnce(t, NewService(cfg, []string{"sk-test"}))
}

func TestBillingFailedAttemptDoesNotReplaceRecordedSuccess(t *testing.T) {
	svc := NewService(failoverBillingConfig(t.TempDir()+"/billing.jsonl"), []string{"sk-test"})
	p, ok := svc.AuthenticateToken("sk-test")
	if !ok {
		t.Fatal("legacy token did not authenticate")
	}
	ctx := WithPrincipal(context.Background(), p)
	svc.HandleUsage(ctx, coreusage.Record{RequestID: "req", Provider: "openai", Model: "gpt-5.5", AuthIndex: "first", RequestedAt: time.Now(), Detail: coreusage.Detail{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}})
	svc.HandleUsage(ctx, coreusage.Record{RequestID: "req", Provider: "openai", Model: "gpt-5.5", AuthIndex: "late-failure", Failed: true, RequestedAt: time.Now()})
	svc.HandleUsage(ctx, coreusage.Record{RequestID: "req", Provider: "openai", Model: "gpt-5.5", AuthIndex: "duplicate", RequestedAt: time.Now(), Detail: coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})

	summary := svc.Summary()
	if summary.Requests != 1 || summary.CustomerCostNanos != 100*10+10*100 {
		t.Fatalf("summary requests=%d cost=%d, want the first success only", summary.Requests, summary.CustomerCostNanos)
	}
	if event, _ := svc.EventByRequestID("req"); event.AuthIndex != "first" {
		t.Fatalf("recorded attempt = %q, want first", event.AuthIndex)
	}
}
