package billing

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const postgresTestDSNEnv = "BILLING_TEST_POSTGRES_DSN"

func newPostgresBillingTestService(t *testing.T) (*Service, Config) {
	t.Helper()
	if os.Getenv(postgresTestDSNEnv) == "" {
		t.Skipf("%s is not set", postgresTestDSNEnv)
	}
	cfg := failoverBillingConfig(t.TempDir() + "/billing.jsonl")
	cfg.Postgres = PostgresConfig{DSNEnv: postgresTestDSNEnv, Schema: fmt.Sprintf("billing_test_%d", time.Now().UnixNano())}
	svc := NewService(cfg, []string{"sk-test"})
	if svc.pg == nil {
		t.Fatal("postgres pool was not connected")
	}
	t.Cleanup(func() {
		_, _ = svc.pg.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, quoteIdent(svc.pgSchema)))
		svc.pg.Close()
	})
	return svc, cfg
}

type rollupRow struct {
	requests, customer, upstream int64
}

func readRollup(t *testing.T, svc *Service, day string) rollupRow {
	t.Helper()
	var row rollupRow
	err := svc.pg.QueryRow(context.Background(), fmt.Sprintf(`SELECT COALESCE(SUM(requests),0), COALESCE(SUM(customer_cost_nanos),0), COALESCE(SUM(upstream_equivalent_cost_nanos),0) FROM %s.usage_rollup_daily WHERE day = $1`, quoteIdent(svc.pgSchema)), day).Scan(&row.requests, &row.customer, &row.upstream)
	if err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	return row
}

func TestBillingPostgresSuccessfulFailoverAttemptSupersedesFailedAttempt(t *testing.T) {
	svc, cfg := newPostgresBillingTestService(t)
	p, ok := svc.AuthenticateToken("sk-test")
	if !ok {
		t.Fatal("legacy token did not authenticate")
	}
	ctx := WithPrincipal(context.Background(), p)
	svc.HandleUsage(ctx, coreusage.Record{RequestID: "req", Provider: "openai", Model: "gpt-5.5", AuthIndex: "overloaded", Failed: true, RequestedAt: time.Now(), Detail: coreusage.Detail{InputTokens: 100}})
	svc.HandleUsage(ctx, coreusage.Record{RequestID: "req", Provider: "openai", Model: "gpt-5.5", AuthIndex: "rescuer", RequestedAt: time.Now(), Detail: coreusage.Detail{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}})
	svc.HandleUsage(ctx, coreusage.Record{RequestID: "req", Provider: "openai", Model: "gpt-5.5", AuthIndex: "late-failure", Failed: true, RequestedAt: time.Now()})

	const wantCost = int64(100*10 + 10*100)
	var rows int
	var customer int64
	var failed bool
	var authIndex string
	err := svc.pg.QueryRow(context.Background(), fmt.Sprintf(`SELECT count(*) OVER (), customer_cost_nanos, (payload->>'failed')::boolean, payload->>'auth_index' FROM %s.billing_events`, quoteIdent(svc.pgSchema))).Scan(&rows, &customer, &failed, &authIndex)
	if err != nil {
		t.Fatalf("read billing event: %v", err)
	}
	if rows != 1 || customer != wantCost || failed || authIndex != "rescuer" {
		t.Fatalf("billing_events rows=%d customer=%d failed=%v auth=%q, want one rescuer row costing %d", rows, customer, failed, authIndex, wantCost)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if got := readRollup(t, svc, today); got != (rollupRow{requests: 1, customer: wantCost, upstream: wantCost}) {
		t.Fatalf("rollup = %+v, want one request costing %d", got, wantCost)
	}

	restarted := NewService(cfg, []string{"sk-test"})
	defer restarted.pg.Close()
	if summary := restarted.Summary(); summary.Requests != 1 || summary.CustomerCostNanos != wantCost {
		t.Fatalf("restarted summary requests=%d cost=%d, want 1 request costing %d", summary.Requests, summary.CustomerCostNanos, wantCost)
	}
}

func TestBillingPostgresLedgerReplayBackfillsMissingEventsAndSupersedes(t *testing.T) {
	svc, cfg := newPostgresBillingTestService(t)
	p, ok := svc.AuthenticateToken("sk-test")
	if !ok {
		t.Fatal("legacy token did not authenticate")
	}
	ctx := WithPrincipal(context.Background(), p)
	svc.HandleUsage(ctx, coreusage.Record{RequestID: "kept", Provider: "openai", Model: "gpt-5.5", RequestedAt: time.Now(), Detail: coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
	svc.HandleUsage(ctx, coreusage.Record{RequestID: "rescued", Provider: "openai", Model: "gpt-5.5", Failed: true, RequestedAt: time.Now()})

	// Simulate events that reached only the JSONL ledger while PostgreSQL was unavailable.
	offline := NewService(Config{Enabled: true, Mode: "observe", LedgerPath: cfg.LedgerPath, Token: cfg.Token, PriceBook: cfg.PriceBook, Postgres: PostgresConfig{DSNEnv: "BILLING_TEST_UNSET_DSN"}}, []string{"sk-test"})
	offline.HandleUsage(ctx, coreusage.Record{RequestID: "rescued", Provider: "openai", Model: "gpt-5.5", RequestedAt: time.Now(), Detail: coreusage.Detail{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}})
	offline.HandleUsage(ctx, coreusage.Record{RequestID: "offline", Provider: "openai", Model: "gpt-5.5", RequestedAt: time.Now(), Detail: coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})

	restarted := NewService(cfg, []string{"sk-test"})
	defer restarted.pg.Close()
	rows, err := svc.pg.Query(context.Background(), fmt.Sprintf(`SELECT request_id, (payload->>'failed')::boolean FROM %s.billing_events ORDER BY request_id`, quoteIdent(svc.pgSchema)))
	if err != nil {
		t.Fatalf("read billing events: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var requestID string
		var failed bool
		if err := rows.Scan(&requestID, &failed); err != nil {
			t.Fatal(err)
		}
		got[requestID] = failed
	}
	want := map[string]bool{"kept": false, "offline": false, "rescued": false}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("billing_events after replay = %v, want %v", got, want)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if rollup := readRollup(t, svc, today); rollup.requests != 3 {
		t.Fatalf("rollup requests = %d, want 3", rollup.requests)
	}
}

func TestBillingPostgresSupersedeMovesRollupAcrossDays(t *testing.T) {
	svc, _ := newPostgresBillingTestService(t)
	now := time.Now().UTC()
	yesterday := now.Add(-24 * time.Hour)
	failedUpstream, zero := int64(7), int64(0)
	failedAttempt := Event{EventID: "evt", RequestID: "req", UserID: "u", TokenID: "t", Provider: "openai", Model: "gpt-5.5", Failed: true, RequestedAt: yesterday, CompletedAt: yesterday, CustomerCostNanos: &zero, UpstreamEquivalentCostNanos: &failedUpstream}
	cost := int64(42)
	success := failedAttempt
	success.Failed = false
	success.CompletedAt = now
	success.CustomerCostNanos = &cost
	success.UpstreamEquivalentCostNanos = &cost

	if err := svc.appendPostgresLocked(failedAttempt); err != nil {
		t.Fatalf("append failed attempt: %v", err)
	}
	if err := svc.appendPostgresLocked(success); err != nil {
		t.Fatalf("append success: %v", err)
	}
	if err := svc.appendPostgresLocked(success); err != nil {
		t.Fatalf("replay success: %v", err)
	}
	if got := readRollup(t, svc, yesterday.Format("2006-01-02")); got != (rollupRow{}) {
		t.Fatalf("yesterday rollup = %+v, want zeroed", got)
	}
	if got := readRollup(t, svc, now.Format("2006-01-02")); got != (rollupRow{requests: 1, customer: cost, upstream: cost}) {
		t.Fatalf("today rollup = %+v, want one request costing %d", got, cost)
	}
}
