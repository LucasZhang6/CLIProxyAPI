package billing

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	ProviderName              = "billing-api-token"
	modeObserve               = "observe"
	modeEnforceSoft           = "enforce-soft"
	modeEnforceHard           = "enforce-hard"
	policySuccessFinalAttempt = "success_final_attempt_v1"

	unknownPricePolicyRecordUnpriced       = "record-unpriced"
	unknownPricePolicyReject               = "reject"
	unknownPricePolicyFallbackHighestKnown = "fallback-highest-known"
)

type tokenRuntime struct {
	id        string
	userID    string
	name      string
	hint      string
	plain     string
	digest    []byte
	status    string
	expiresAt *time.Time
	quota     QuotaConfig
	rate      RateLimitConfig
}

type userRuntime struct {
	id     string
	name   string
	email  string
	status string
	quota  QuotaConfig
	rate   RateLimitConfig
}

type limiterState struct {
	windowStart time.Time
	count       int
	inflight    int
}

type Service struct {
	mu              sync.Mutex
	cfg             Config
	pepper          string
	users           map[string]*userRuntime
	tokensByID      map[string]*tokenRuntime
	tokensByPlain   map[string]*tokenRuntime
	legacyKeys      map[string]*tokenRuntime
	priceRules      []PriceRule
	events          []Event
	seen            map[string]int
	limiters        map[string]*limiterState
	spendDay        map[string]int64
	spendMonth      map[string]int64
	spendDayToken   map[string]int64
	spendMonthToken map[string]int64
	ledgerPath      string
	pg              *pgxpool.Pool
	pgSchema        string
}

func NewService(cfg Config, legacyAPIKeys []string) *Service {
	normalizeConfig(&cfg)
	s := &Service{seen: map[string]int{}, limiters: map[string]*limiterState{}, spendDay: map[string]int64{}, spendMonth: map[string]int64{}, spendDayToken: map[string]int64{}, spendMonthToken: map[string]int64{}, ledgerPath: strings.TrimSpace(cfg.LedgerPath)}
	s.applyConfigLocked(cfg, legacyAPIKeys)
	s.connectPostgres()
	if s.pg != nil {
		s.loadPostgresLedger()
	}
	if s.ledgerPath != "" {
		_ = os.MkdirAll(filepath.Dir(s.ledgerPath), 0o755)
		s.loadLedger()
	}
	return s
}

// ApplyConfig refreshes billing identity, quota, rate, and price-book runtime
// state after a management or filesystem config reload. It deliberately keeps
// recorded events, dedupe state, loaded spend counters, and the existing
// storage connection intact.
func (s *Service) ApplyConfig(cfg Config, legacyAPIKeys []string) {
	if s == nil {
		return
	}
	normalizeConfig(&cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyConfigLocked(cfg, legacyAPIKeys)
}

func (s *Service) applyConfigLocked(cfg Config, legacyAPIKeys []string) {
	s.cfg = cfg
	s.pepper = os.Getenv(strings.TrimSpace(cfg.Token.PepperEnv))
	s.users = map[string]*userRuntime{}
	s.tokensByID = map[string]*tokenRuntime{}
	s.tokensByPlain = map[string]*tokenRuntime{}
	s.legacyKeys = map[string]*tokenRuntime{}
	s.priceRules = defaultPriceRules(cfg.PriceBook)
	s.ledgerPath = strings.TrimSpace(cfg.LedgerPath)
	for _, u := range cfg.Users {
		uid := strings.TrimSpace(u.ID)
		if uid == "" {
			continue
		}
		status := strings.TrimSpace(u.Status)
		if status == "" {
			status = "active"
		}
		s.users[uid] = &userRuntime{id: uid, name: strings.TrimSpace(u.Name), email: strings.TrimSpace(u.Email), status: status, quota: mergeQuota(cfg.Quota, u.Quota), rate: mergeRate(cfg.RateLimit, u.Rate)}
		for _, tc := range u.Tokens {
			s.addTokenLocked(uid, tc, s.users[uid].quota, s.users[uid].rate)
		}
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Token.LegacyAPIKeys), "observe") || strings.EqualFold(strings.TrimSpace(cfg.Token.LegacyAPIKeys), "allow") {
		legacyUser := "legacy"
		if _, ok := s.users[legacyUser]; !ok {
			s.users[legacyUser] = &userRuntime{id: legacyUser, name: "Legacy API keys", status: "active", quota: cfg.Quota, rate: cfg.RateLimit}
		}
		for i, key := range legacyAPIKeys {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			tok := &tokenRuntime{id: fmt.Sprintf("legacy-%d", i+1), userID: legacyUser, name: "legacy", hint: hintToken(key), plain: key, status: "active", quota: cfg.Quota, rate: cfg.RateLimit}
			s.legacyKeys[key] = tok
		}
	}
}

func (s *Service) connectPostgres() {
	if s == nil {
		return
	}
	dsn := strings.TrimSpace(os.Getenv(strings.TrimSpace(s.cfg.Postgres.DSNEnv)))
	if dsn == "" {
		return
	}
	schema := sanitizeSchema(s.cfg.Postgres.Schema)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "billing postgres connect failed: %v\n", err)
		return
	}
	if err = pool.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "billing postgres ping failed: %v\n", err)
		pool.Close()
		return
	}
	s.pg = pool
	s.pgSchema = schema
	if err = s.migratePostgres(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "billing postgres migrate failed: %v\n", err)
		pool.Close()
		s.pg = nil
		return
	}
}

func sanitizeSchema(schema string) string {
	schema = strings.TrimSpace(schema)
	if schema == "" {
		return "billing"
	}
	for _, r := range schema {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return "billing"
	}
	return schema
}

func quoteIdent(ident string) string { return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"` }

func (s *Service) migratePostgres(ctx context.Context) error {
	if s == nil || s.pg == nil {
		return nil
	}
	schema := quoteIdent(s.pgSchema)
	stmts := []string{
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, schema),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.billing_events (
			event_id text PRIMARY KEY,
			request_id text NOT NULL,
			user_id text NOT NULL,
			token_id text NOT NULL,
			provider text NOT NULL,
			model text NOT NULL,
			customer_cost_nanos bigint,
			upstream_equivalent_cost_nanos bigint,
			requested_at timestamptz NOT NULL,
			completed_at timestamptz NOT NULL,
			payload jsonb NOT NULL
		)`, schema),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS billing_events_completed_at_idx ON %s.billing_events (completed_at DESC)`, schema),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS billing_events_request_id_idx ON %s.billing_events (request_id)`, schema),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.usage_rollup_daily (
			day date NOT NULL,
			user_id text NOT NULL,
			token_id text NOT NULL,
			requests bigint NOT NULL DEFAULT 0,
			customer_cost_nanos bigint NOT NULL DEFAULT 0,
			upstream_equivalent_cost_nanos bigint NOT NULL DEFAULT 0,
			PRIMARY KEY (day, user_id, token_id)
		)`, schema),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.quota_reservations (
			reservation_id text PRIMARY KEY,
			user_id text NOT NULL,
			token_id text NOT NULL,
			estimated_cost_nanos bigint NOT NULL,
			status text NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(),
			expires_at timestamptz NOT NULL
		)`, schema),
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) loadPostgresLedger() {
	if s == nil || s.pg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := s.pg.Query(ctx, fmt.Sprintf(`SELECT payload FROM %s.billing_events ORDER BY completed_at ASC`, quoteIdent(s.pgSchema)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "billing postgres load failed: %v\n", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var event Event
		if err := json.Unmarshal(payload, &event); err != nil || event.EventID == "" {
			continue
		}
		s.recordLoadedEventLocked(event)
	}
}

func (s *Service) loadLedger() {
	if s == nil || s.ledgerPath == "" {
		return
	}
	file, err := os.Open(s.ledgerPath)
	if err != nil {
		return
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil || event.EventID == "" {
			continue
		}
		// Events already loaded are in PostgreSQL; replay only backfills missing events and
		// successes that supersede a stored failed attempt.
		idx, known := s.seen[event.EventID]
		replay := !known || supersedesFailedAttempt(s.events[idx], event)
		s.recordLoadedEventLocked(event)
		if s.pg != nil && replay {
			_ = s.appendPostgresLocked(event)
		}
	}
}

// supersedesFailedAttempt reports whether next should replace the recorded
// event with the same ID. Failover attempts of one request share an event ID;
// the request is billed by its final successful attempt.
func supersedesFailedAttempt(recorded, next Event) bool {
	return recorded.Failed && !next.Failed
}

func (s *Service) recordLoadedEventLocked(event Event) {
	if idx, exists := s.seen[event.EventID]; exists {
		if !supersedesFailedAttempt(s.events[idx], event) {
			return
		}
		s.addLoadedSpendLocked(s.events[idx], -1)
		s.events[idx] = event
	} else {
		s.seen[event.EventID] = len(s.events)
		s.events = append(s.events, event)
	}
	s.addLoadedSpendLocked(event, 1)
}

func (s *Service) addLoadedSpendLocked(event Event, sign int64) {
	if event.CustomerCostNanos == nil {
		return
	}
	cost := sign * *event.CustomerCostNanos
	now := time.Now().UTC()
	completed := event.CompletedAt.UTC()
	if completed.Format("2006-01-02") == now.Format("2006-01-02") {
		s.spendDay[event.UserID] += cost
		s.spendDayToken[tokenSpendKey(event.UserID, event.TokenID)] += cost
	}
	if completed.Format("2006-01") == now.Format("2006-01") {
		s.spendMonth[event.UserID] += cost
		s.spendMonthToken[tokenSpendKey(event.UserID, event.TokenID)] += cost
	}
}

func (s *Service) appendPostgresLocked(event Event) error {
	if s == nil || s.pg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	customer := int64(0)
	if event.CustomerCostNanos != nil {
		customer = *event.CustomerCostNanos
	}
	upstream := int64(0)
	if event.UpstreamEquivalentCostNanos != nil {
		upstream = *event.UpstreamEquivalentCostNanos
	}
	schema := quoteIdent(s.pgSchema)
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.billing_events (event_id, request_id, user_id, token_id, provider, model, customer_cost_nanos, upstream_equivalent_cost_nanos, requested_at, completed_at, payload) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (event_id) DO NOTHING`, schema), event.EventID, event.RequestID, event.UserID, event.TokenID, event.Provider, event.Model, nullableInt64(event.CustomerCostNanos), nullableInt64(event.UpstreamEquivalentCostNanos), event.RequestedAt, event.CompletedAt, payload)
	if err != nil {
		return err
	}
	day := event.CompletedAt.UTC().Format("2006-01-02")
	if tag.RowsAffected() > 0 {
		if err = addUsageRollup(ctx, tx, schema, day, event.UserID, event.TokenID, 1, customer, upstream); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if event.Failed {
		return nil
	}
	var oldCustomer, oldUpstream *int64
	var oldCompletedAt time.Time
	var oldFailed bool
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT customer_cost_nanos, upstream_equivalent_cost_nanos, completed_at, COALESCE((payload->>'failed')::boolean, false) FROM %s.billing_events WHERE event_id = $1 FOR UPDATE`, schema), event.EventID).Scan(&oldCustomer, &oldUpstream, &oldCompletedAt, &oldFailed)
	if err != nil {
		return err
	}
	if !oldFailed {
		return nil
	}
	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.billing_events SET request_id = $2, user_id = $3, token_id = $4, provider = $5, model = $6, customer_cost_nanos = $7, upstream_equivalent_cost_nanos = $8, requested_at = $9, completed_at = $10, payload = $11 WHERE event_id = $1`, schema), event.EventID, event.RequestID, event.UserID, event.TokenID, event.Provider, event.Model, nullableInt64(event.CustomerCostNanos), nullableInt64(event.UpstreamEquivalentCostNanos), event.RequestedAt, event.CompletedAt, payload)
	if err != nil {
		return err
	}
	oldCustomerValue, oldUpstreamValue := int64(0), int64(0)
	if oldCustomer != nil {
		oldCustomerValue = *oldCustomer
	}
	if oldUpstream != nil {
		oldUpstreamValue = *oldUpstream
	}
	oldDay := oldCompletedAt.UTC().Format("2006-01-02")
	if oldDay == day {
		err = addUsageRollup(ctx, tx, schema, day, event.UserID, event.TokenID, 0, customer-oldCustomerValue, upstream-oldUpstreamValue)
	} else {
		err = addUsageRollup(ctx, tx, schema, oldDay, event.UserID, event.TokenID, -1, -oldCustomerValue, -oldUpstreamValue)
		if err == nil {
			err = addUsageRollup(ctx, tx, schema, day, event.UserID, event.TokenID, 1, customer, upstream)
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func addUsageRollup(ctx context.Context, tx pgx.Tx, schema, day, userID, tokenID string, requests, customer, upstream int64) error {
	_, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.usage_rollup_daily (day, user_id, token_id, requests, customer_cost_nanos, upstream_equivalent_cost_nanos) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (day, user_id, token_id) DO UPDATE SET requests = usage_rollup_daily.requests + EXCLUDED.requests, customer_cost_nanos = usage_rollup_daily.customer_cost_nanos + EXCLUDED.customer_cost_nanos, upstream_equivalent_cost_nanos = usage_rollup_daily.upstream_equivalent_cost_nanos + EXCLUDED.upstream_equivalent_cost_nanos`, schema), day, userID, tokenID, requests, customer, upstream)
	return err
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func (s *Service) appendLedgerLocked(event Event) error {
	if err := s.appendPostgresLocked(event); err != nil {
		return err
	}
	if s == nil || s.ledgerPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.ledgerPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(s.ledgerPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err = file.Write(append(payload, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func normalizeConfig(cfg *Config) {
	cfg.Mode = strings.TrimSpace(cfg.Mode)
	if cfg.Mode == "" {
		cfg.Mode = modeObserve
	}
	cfg.Currency = strings.TrimSpace(cfg.Currency)
	if cfg.Currency == "" {
		cfg.Currency = "USD"
	}
	cfg.UnknownPricePolicy = normalizeUnknownPricePolicy(cfg.UnknownPricePolicy)
	if cfg.Token.PepperEnv == "" {
		cfg.Token.PepperEnv = "BILLING_TOKEN_PEPPER"
	}
	if cfg.Token.LegacyAPIKeys == "" {
		cfg.Token.LegacyAPIKeys = "observe"
	}
	if strings.TrimSpace(cfg.LedgerPath) == "" {
		cfg.LedgerPath = "billing-ledger.jsonl"
	}
	if strings.TrimSpace(cfg.Postgres.DSNEnv) == "" {
		cfg.Postgres.DSNEnv = "BILLING_PG_DSN"
	}
	if strings.TrimSpace(cfg.Postgres.Schema) == "" {
		cfg.Postgres.Schema = "billing"
	}
	if strings.TrimSpace(cfg.PriceBook.Version) == "" {
		cfg.PriceBook.Version = defaultPriceBookVersion
	}
}

func normalizeUnknownPricePolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", unknownPricePolicyRecordUnpriced, "record", "observe":
		return unknownPricePolicyRecordUnpriced
	case unknownPricePolicyReject, "deny", "block", "fail", "fail-closed":
		return unknownPricePolicyReject
	case unknownPricePolicyFallbackHighestKnown, "fallback", "fallback-highest", "highest-known":
		return unknownPricePolicyFallbackHighestKnown
	default:
		return unknownPricePolicyRecordUnpriced
	}
}

func (s *Service) addTokenLocked(userID string, tc TokenConfigEntry, defaultQuota QuotaConfig, defaultRate RateLimitConfig) {
	id := strings.TrimSpace(tc.ID)
	if id == "" {
		return
	}
	status := strings.TrimSpace(tc.Status)
	if status == "" {
		status = "active"
	}
	tok := &tokenRuntime{id: id, userID: userID, name: strings.TrimSpace(tc.Name), hint: strings.TrimSpace(tc.Hint), plain: strings.TrimSpace(tc.Token), status: status, expiresAt: tc.ExpiresAt, quota: mergeQuota(defaultQuota, tc.Quota), rate: mergeRate(defaultRate, tc.Rate)}
	if tok.hint == "" && tok.plain != "" {
		tok.hint = hintToken(tok.plain)
	}
	if digest := strings.TrimSpace(tc.TokenDigest); digest != "" {
		if b, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:")); err == nil {
			tok.digest = b
		}
	}
	if tok.digest == nil && tok.plain != "" && s.pepper != "" {
		tok.digest = hmacDigest(s.pepper, tok.plain)
	}
	s.tokensByID[id] = tok
	if tok.plain != "" {
		s.tokensByPlain[tok.plain] = tok
	}
}

func (s *Service) Enabled() bool { return s != nil && s.cfg.Enabled }
func (s *Service) Mode() string {
	if s == nil {
		return ""
	}
	return s.cfg.Mode
}

func (s *Service) AuthenticateToken(token string) (Principal, bool) {
	if !s.Enabled() {
		return Principal{}, false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return Principal{}, false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok := s.lookupTokenLocked(token); tok != nil && s.tokenActiveLocked(tok, now) {
		return Principal{UserID: tok.userID, TokenID: tok.id, Provider: ProviderName}, true
	}
	if tok := s.legacyKeys[token]; tok != nil && s.tokenActiveLocked(tok, now) {
		return Principal{UserID: tok.userID, TokenID: tok.id, Provider: "config-inline-legacy"}, true
	}
	return Principal{}, false
}

func (s *Service) lookupTokenLocked(token string) *tokenRuntime {
	if tok := s.tokensByPlain[token]; tok != nil {
		return tok
	}
	if strings.HasPrefix(token, "cpa_sk_") {
		parts := strings.SplitN(token, ".", 2)
		prefix := strings.TrimPrefix(parts[0], "cpa_sk_live_")
		prefix = strings.TrimPrefix(prefix, "cpa_sk_test_")
		if tok := s.tokensByID[prefix]; tok != nil && tok.digest != nil && s.pepper != "" && subtle.ConstantTimeCompare(tok.digest, hmacDigest(s.pepper, token)) == 1 {
			return tok
		}
	}
	return nil
}

func (s *Service) tokenActiveLocked(tok *tokenRuntime, now time.Time) bool {
	if tok == nil || !strings.EqualFold(tok.status, "active") {
		return false
	}
	user := s.users[tok.userID]
	if user == nil || !strings.EqualFold(user.status, "active") {
		return false
	}
	if tok.expiresAt != nil && now.After(*tok.expiresAt) {
		return false
	}
	return true
}

func (s *Service) GuardMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.Enabled() {
			c.Next()
			return
		}
		p := PrincipalFromGin(c)
		if p.UserID == "" {
			if parsed, ok := s.AuthenticateToken(extractToken(c.Request)); ok {
				p = parsed
				SetGinPrincipal(c, p)
			}
		}
		if p.UserID == "" {
			c.Next()
			return
		}
		if err := s.rejectUnknownPricedRequestModel(c); err != nil {
			c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{"error": gin.H{"type": "billing_error", "code": "unpriced_model", "message": err.Error()}})
			return
		}
		lease, err := s.acquire(p)
		if err != nil {
			c.Header("Retry-After", "60")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"type": "rate_limit_error", "code": "quota_exceeded", "message": err.Error()}})
			return
		}
		defer s.release(lease)
		c.Next()
	}
}

func (s *Service) rejectUnknownPricedRequestModel(c *gin.Context) error {
	if s == nil || c == nil || c.Request == nil || strings.TrimSpace(s.cfg.UnknownPricePolicy) != unknownPricePolicyReject {
		return nil
	}
	if c.Request.Body == nil || c.Request.Body == http.NoBody {
		return nil
	}
	method := strings.ToUpper(strings.TrimSpace(c.Request.Method))
	if method != http.MethodPost && method != http.MethodPut && method != http.MethodPatch {
		return nil
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil
	}
	s.mu.Lock()
	_, ok := s.resolvePriceLocked("", model)
	s.mu.Unlock()
	if ok {
		return nil
	}
	return fmt.Errorf("billing price is not configured for model %q", model)
}

type lease struct {
	key    string
	active bool
}

func (s *Service) acquire(p Principal) (lease, error) {
	if s.cfg.Mode == modeObserve {
		return lease{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := s.tokensByID[p.TokenID]
	rate := s.cfg.RateLimit
	quota := s.cfg.Quota
	spendDay := s.spendDay[p.UserID]
	spendMonth := s.spendMonth[p.UserID]
	if tok != nil {
		rate = mergeRate(rate, tok.rate)
		quota = mergeQuota(quota, tok.quota)
		key := tokenSpendKey(p.UserID, p.TokenID)
		spendDay = s.spendDayToken[key]
		spendMonth = s.spendMonthToken[key]
	} else if user := s.users[p.UserID]; user != nil {
		rate = mergeRate(rate, user.rate)
		quota = mergeQuota(quota, user.quota)
	}
	if quota.DailyNanos > 0 && spendDay >= quota.DailyNanos {
		return lease{}, fmt.Errorf("billing daily quota exceeded")
	}
	if quota.MonthlyNanos > 0 && spendMonth >= quota.MonthlyNanos {
		return lease{}, fmt.Errorf("billing monthly quota exceeded")
	}
	key := p.UserID + ":" + p.TokenID
	st := s.limiters[key]
	if st == nil {
		st = &limiterState{windowStart: time.Now()}
		s.limiters[key] = st
	}
	now := time.Now()
	if now.Sub(st.windowStart) >= time.Minute {
		st.windowStart = now
		st.count = 0
	}
	if rate.RPM > 0 && st.count >= rate.RPM {
		return lease{}, fmt.Errorf("billing RPM limit exceeded")
	}
	if rate.Concurrent > 0 && st.inflight >= rate.Concurrent {
		return lease{}, fmt.Errorf("billing concurrent limit exceeded")
	}
	st.count++
	st.inflight++
	return lease{key: key, active: true}, nil
}
func (s *Service) release(l lease) {
	if !l.active {
		return
	}
	s.mu.Lock()
	if st := s.limiters[l.key]; st != nil && st.inflight > 0 {
		st.inflight--
	}
	s.mu.Unlock()
}

func (s *Service) HandleUsage(ctx context.Context, record coreusage.Record) {
	if !s.Enabled() {
		return
	}
	p := PrincipalFromContext(ctx)
	if p.UserID == "" && strings.TrimSpace(record.UserID) != "" {
		p = Principal{UserID: strings.TrimSpace(record.UserID), TokenID: strings.TrimSpace(record.TokenID), TeamID: strings.TrimSpace(record.TeamID), Provider: ProviderName}
	}
	if p.UserID == "" {
		p = Principal{UserID: "legacy", TokenID: "unknown", Provider: "unknown"}
	}
	if p.TokenID == "" || p.TokenID == "unknown" {
		if strings.EqualFold(p.UserID, "legacy") && len(s.legacyKeys) == 1 {
			for _, tok := range s.legacyKeys {
				p.TokenID = tok.id
			}
		}
	}
	if p.TokenID == "" {
		p.TokenID = "unknown"
	}
	reqID := strings.TrimSpace(record.RequestID)
	if reqID == "" {
		reqID = fmt.Sprintf("billing-%d", time.Now().UnixNano())
	}
	eventID := hashID(p.UserID + "|" + p.TokenID + "|" + reqID + "|" + record.Provider + "|" + record.Model)
	e := Event{EventID: eventID, RequestID: reqID, UserID: p.UserID, TokenID: p.TokenID, Provider: record.Provider, Model: record.Model, Alias: record.Alias, AuthIndex: record.AuthIndex, SessionID: record.SessionID, Failed: record.Failed, BillingPolicy: policySuccessFinalAttempt, RequestedAt: record.RequestedAt, CompletedAt: time.Now(), LatencyMs: record.Latency.Milliseconds(), TotalTokens: record.Detail.TotalTokens}
	if e.RequestedAt.IsZero() {
		e.RequestedAt = e.CompletedAt
	}
	e.InputCacheReadTokens = max64(record.Detail.CacheReadTokens, record.Detail.CachedTokens)
	input := record.Detail.InputTokens
	if input == 0 && record.Detail.TokenBreakdown.Valid() {
		input = record.Detail.TokenBreakdown.Input.TotalTokens
	}
	e.InputUncachedTokens = input - e.InputCacheReadTokens - record.Detail.CacheCreationTokens
	if e.InputUncachedTokens < 0 {
		e.InputUncachedTokens = 0
	}
	// MVP: when TTL is unknown, bill cache creation at 5m price and mark estimated.
	e.InputCacheWrite5mTokens = record.Detail.CacheCreationTokens
	e.OutputReasoningTokens = record.Detail.ReasoningTokens
	e.OutputTextTokens = record.Detail.OutputTokens - record.Detail.ReasoningTokens
	if e.OutputTextTokens < 0 {
		e.OutputTextTokens = record.Detail.OutputTokens
	}
	if record.Detail.CacheCreationTokens > 0 {
		e.BillingQuality = "estimated"
	} else {
		e.BillingQuality = "exact"
	}
	s.priceAndRecord(&e)
}

func (s *Service) priceAndRecord(e *Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, exists := s.seen[e.EventID]
	if exists && !supersedesFailedAttempt(s.events[idx], *e) {
		return
	}
	rule, ok := s.resolvePriceLocked(e.Provider, e.Model)
	fallback := false
	if !ok && strings.TrimSpace(s.cfg.UnknownPricePolicy) == unknownPricePolicyFallbackHighestKnown {
		if fallbackRule, okFallback := s.resolveFallbackPriceLocked(); okFallback {
			rule = fallbackRule
			ok = true
			fallback = true
		}
	}
	if ok {
		cost := e.InputUncachedTokens*rule.InputUncachedNanosPerToken + e.InputCacheReadTokens*rule.InputCacheReadNanosPerToken + e.InputCacheWrite5mTokens*rule.InputCacheWrite5mNanosPerToken + e.InputCacheWrite1hTokens*rule.InputCacheWrite1hNanosPerToken + e.OutputTextTokens*rule.OutputTextNanosPerToken + e.OutputReasoningTokens*rule.OutputReasoningNanosPerToken
		e.PriceBookVersion = s.cfg.PriceBook.Version
		e.PriceRuleID = rule.ID
		snap := rule
		if fallback {
			e.BillingQuality = "fallback"
			e.PriceRuleID = "fallback-highest-known:" + rule.ID
			snap.ID = e.PriceRuleID
		}
		e.PriceSnapshot = &snap
		e.UpstreamEquivalentCostNanos = &cost
		customer := int64(0)
		if !e.Failed {
			customer = cost
		}
		e.CustomerCostNanos = &customer
	} else {
		e.BillingQuality = "unpriced"
	}
	if exists {
		s.addSpendLocked(s.events[idx], -1)
		s.events[idx] = *e
	} else {
		s.seen[e.EventID] = len(s.events)
		s.events = append(s.events, *e)
	}
	if err := s.appendLedgerLocked(*e); err != nil {
		fmt.Fprintf(os.Stderr, "billing ledger append failed: %v\n", err)
	}
	s.addSpendLocked(*e, 1)
}

func (s *Service) addSpendLocked(e Event, sign int64) {
	if e.CustomerCostNanos == nil {
		return
	}
	cost := sign * *e.CustomerCostNanos
	s.spendDay[e.UserID] += cost
	s.spendMonth[e.UserID] += cost
	key := tokenSpendKey(e.UserID, e.TokenID)
	s.spendDayToken[key] += cost
	s.spendMonthToken[key] += cost
}

func tokenSpendKey(userID, tokenID string) string {
	return strings.TrimSpace(userID) + ":" + strings.TrimSpace(tokenID)
}

func (s *Service) resolvePriceLocked(provider, model string) (PriceRule, bool) {
	p := strings.ToLower(strings.TrimSpace(provider))
	m := strings.ToLower(strings.TrimSpace(model))
	for _, r := range s.priceRules {
		if p != "" && strings.EqualFold(r.BillingProvider, p) && strings.EqualFold(r.Model, m) {
			return r, true
		}
	}
	for _, r := range s.priceRules {
		if strings.EqualFold(r.Model, m) {
			return r, true
		}
	}
	return PriceRule{}, false
}

func (s *Service) resolveFallbackPriceLocked() (PriceRule, bool) {
	var best PriceRule
	var bestScore int64
	for _, r := range s.priceRules {
		score := r.InputUncachedNanosPerToken + r.InputCacheReadNanosPerToken + r.InputCacheWrite5mNanosPerToken + r.InputCacheWrite1hNanosPerToken + r.OutputTextNanosPerToken + r.OutputReasoningNanosPerToken
		if best.ID == "" || score > bestScore {
			best = r
			bestScore = score
		}
	}
	return best, best.ID != ""
}

func (s *Service) Summary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Summary{Enabled: s.Enabled(), Mode: s.cfg.Mode, Currency: s.cfg.Currency, PriceBookVersion: s.cfg.PriceBook.Version, Users: map[string]UserSummary{}}
	for _, e := range s.events {
		out.Requests++
		us := out.Users[e.UserID]
		if us.Tokens == nil {
			us.Tokens = map[string]TokenSummary{}
		}
		ts := us.Tokens[e.TokenID]
		us.Requests++
		ts.Requests++
		if e.CustomerCostNanos != nil {
			out.PricedRequests++
			out.CustomerCostNanos += *e.CustomerCostNanos
			us.CustomerCostNanos += *e.CustomerCostNanos
			ts.CustomerCostNanos += *e.CustomerCostNanos
		}
		if e.UpstreamEquivalentCostNanos != nil {
			out.UpstreamEquivalentCostNanos += *e.UpstreamEquivalentCostNanos
		}
		if e.CustomerCostNanos == nil {
			out.UnpricedRequests++
		}
		us.Tokens[e.TokenID] = ts
		out.Users[e.UserID] = us
	}
	return out
}

func (s *Service) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Event(nil), s.events...)
	sort.Slice(out, func(i, j int) bool { return out[i].CompletedAt.After(out[j].CompletedAt) })
	return out
}

func (s *Service) EventsAfter(afterCompletedAt time.Time, afterEventID string, limit int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0, len(s.events))
	afterEventID = strings.TrimSpace(afterEventID)
	for _, event := range s.events {
		if !afterCompletedAt.IsZero() {
			if event.CompletedAt.Before(afterCompletedAt) {
				continue
			}
			if event.CompletedAt.Equal(afterCompletedAt) && strings.TrimSpace(event.EventID) <= afterEventID {
				continue
			}
		}
		out = append(out, event)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CompletedAt.Equal(out[j].CompletedAt) {
			return out[i].CompletedAt.Before(out[j].CompletedAt)
		}
		return out[i].EventID < out[j].EventID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (s *Service) EventByRequestID(id string) (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.events {
		if s.events[i].RequestID == id {
			return s.events[i], true
		}
	}
	return Event{}, false
}

func hmacDigest(pepper, token string) []byte {
	mac := hmac.New(sha256.New, []byte(pepper))
	mac.Write([]byte(token))
	return mac.Sum(nil)
}
func hashID(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func hintToken(t string) string {
	if len(t) <= 12 {
		return t
	}
	return t[:8] + "..." + t[len(t)-4:]
}
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func mergeQuota(base, override QuotaConfig) QuotaConfig {
	if override.DailyNanos != 0 {
		base.DailyNanos = override.DailyNanos
	}
	if override.MonthlyNanos != 0 {
		base.MonthlyNanos = override.MonthlyNanos
	}
	return base
}
func mergeRate(base, override RateLimitConfig) RateLimitConfig {
	if override.RPM != 0 {
		base.RPM = override.RPM
	}
	if override.Concurrent != 0 {
		base.Concurrent = override.Concurrent
	}
	return base
}
