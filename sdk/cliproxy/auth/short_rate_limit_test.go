package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestShortWindowRateLimitMessageMatching(t *testing.T) {
	codex := &Auth{Provider: "codex"}
	gemini := &Auth{Provider: "gemini"}
	cases := []struct {
		name string
		auth *Auth
		err  *Error
		want bool
	}{
		{
			name: "detail rate limit exceeded",
			auth: codex,
			err:  &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"detail":"Rate limit exceeded"}`},
			want: true,
		},
		{
			name: "rate limit error code",
			auth: codex,
			err:  &Error{HTTPStatus: http.StatusTooManyRequests, Code: "rate_limit_error", Message: "slow down"},
			want: true,
		},
		{
			name: "rate_limit_exceeded",
			auth: codex,
			err:  &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"error":{"code":"rate_limit_exceeded"}}`},
			want: true,
		},
		{
			name: "model rate limit stays on that model",
			auth: codex,
			err:  &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"error":{"type":"rate_limit_error","message":"Model rate limit exceeded"}}`},
			want: false,
		},
		{
			name: "usage limit stays on the quota path",
			auth: codex,
			err:  &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"error":{"type":"usage_limit_reached"}}`},
			want: false,
		},
		{
			name: "bare quota text is not a short window",
			auth: codex,
			err:  &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
			want: false,
		},
		{
			name: "non-codex provider",
			auth: gemini,
			err:  &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"detail":"Rate limit exceeded"}`},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCodexShortWindowRateLimit(tc.auth, tc.err); got != tc.want {
				t.Fatalf("match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMarkResultShortWindowDoesNotRewriteWeeklyQuota(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "codex-a",
		Provider: "codex",
		Status:   StatusActive,
		Quota: QuotaState{
			Signals:    map[string]string{WeeklyQuotaRemainingPercentSignal: "90%"},
			ObservedAt: time.Now(),
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register: %v", errRegister)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-6-sol",
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"detail":"Rate limit exceeded"}`},
	})

	got, ok := manager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatal("auth missing after MarkResult")
	}
	if got.Quota.Reason != shortRateLimitQuotaReason {
		t.Fatalf("reason = %q", got.Quota.Reason)
	}
	if got.Quota.Signals[WeeklyQuotaRemainingPercentSignal] != "90%" {
		t.Fatalf("weekly signal = %v", got.Quota.Signals)
	}
	recoverIn := time.Until(got.Quota.NextRecoverAt)
	if recoverIn < 50*time.Second || recoverIn > 70*time.Second {
		t.Fatalf("recovery in %s, want about 60s", recoverIn)
	}
	blocked, _, _ := isAuthBlockedForModel(got, "gpt-5.6-terra", time.Now())
	if !blocked {
		t.Fatal("short window should block every model on the account")
	}
	for model, state := range got.ModelStates {
		if state != nil && state.Quota.Exceeded {
			t.Fatalf("model %s entered the quota ladder", model)
		}
	}
}

func TestMarkResultUsageLimitStaysPerModel(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "codex-usage", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register: %v", errRegister)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-6-sol",
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"error":{"type":"usage_limit_reached"}}`},
	})
	got, ok := manager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatal("auth missing")
	}
	if got.Quota.Reason == shortRateLimitQuotaReason {
		t.Fatal("usage limit was treated as a short rate limit")
	}
	blocked, _, _ := isAuthBlockedForModel(got, "gpt-5.6-terra", time.Now())
	if blocked {
		t.Fatal("usage-limit cooldown should stay on the model that failed")
	}
}

type shortLimitExecutor struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error
}

func (e *shortLimitExecutor) Identifier() string { return "codex" }
func (e *shortLimitExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls = append(e.calls, auth.ID+"|"+req.Model)
	e.mu.Unlock()
	if errFail := e.fail[auth.ID]; errFail != nil {
		return cliproxyexecutor.Response{}, errFail
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (e *shortLimitExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	_, errExec := e.Execute(ctx, auth, req, opts)
	return nil, errExec
}
func (e *shortLimitExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (e *shortLimitExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *shortLimitExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *shortLimitExecutor) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.calls))
	copy(out, e.calls)
	return out
}

func TestCodexShortWindowRateLimitSwitchesAccountAndSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(3, 30*time.Second, 0)
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &QuotaAwareSelector{},
		TTL:      time.Hour,
	})
	defer affinity.Stop()
	manager.SetSelector(affinity)

	rateLimited := &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"detail":"Rate limit exceeded"}`}
	exec := &shortLimitExecutor{fail: map[string]error{"auth-a": rateLimited}}
	manager.RegisterExecutor(exec)

	modelSol := "gpt-6-sol"
	modelTerra := "gpt-5.6-terra"
	for _, auth := range []*Auth{
		codexQuotaAuth("auth-a", 90),
		codexQuotaAuth("auth-b", 50),
	} {
		if _, errRegister := manager.Register(WithSkipPersist(ctx), auth); errRegister != nil {
			t.Fatalf("Register(%s): %v", auth.ID, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: modelSol}, {ID: modelTerra}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}

	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"sess-short-limit"}}}
	resp, errExec := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: modelSol}, opts)
	if errExec != nil {
		t.Fatalf("first Execute: %v calls=%v", errExec, exec.snapshot())
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if calls := exec.snapshot(); len(calls) != 2 || calls[0] != "auth-a|"+modelSol || calls[1] != "auth-b|"+modelSol {
		t.Fatalf("first request calls = %v", calls)
	}

	if _, errExec = manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: modelSol}, opts); errExec != nil {
		t.Fatalf("same session Execute: %v", errExec)
	}
	if _, errExec = manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: modelTerra}, opts); errExec != nil {
		t.Fatalf("other model Execute: %v", errExec)
	}
	calls := exec.snapshot()
	if len(calls) != 4 || calls[2] != "auth-b|"+modelSol || calls[3] != "auth-b|"+modelTerra {
		t.Fatalf("later calls = %v, want auth-b for both the same session and the other model", calls)
	}

	got, ok := manager.GetByID("auth-a")
	if !ok || got == nil {
		t.Fatal("auth-a missing")
	}
	if got.Quota.Signals[WeeklyQuotaRemainingPercentSignal] != "90%" {
		t.Fatalf("weekly signal rewritten: %v", got.Quota.Signals)
	}
	if got.Quota.Reason != shortRateLimitQuotaReason {
		t.Fatalf("reason = %q", got.Quota.Reason)
	}
}

func TestCodexShortWindowRateLimitReturnsSingleRetryAfter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(3, 30*time.Second, 0)
	manager.SetSelector(&QuotaAwareSelector{})
	rateLimited := &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"detail":"Rate limit exceeded"}`}
	exec := &shortLimitExecutor{fail: map[string]error{"auth-a": rateLimited, "auth-b": rateLimited}}
	manager.RegisterExecutor(exec)

	model := "gpt-6-sol"
	for _, auth := range []*Auth{codexQuotaAuth("auth-a", 90), codexQuotaAuth("auth-b", 50)} {
		if _, errRegister := manager.Register(WithSkipPersist(ctx), auth); errRegister != nil {
			t.Fatalf("Register(%s): %v", auth.ID, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}

	started := time.Now()
	_, errExec := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if time.Since(started) > 2*time.Second {
		t.Fatalf("request waited %s inside the short window", time.Since(started))
	}
	var cooldown *modelCooldownError
	if errExec == nil || !errors.As(errExec, &cooldown) || cooldown == nil {
		t.Fatalf("error = %v, want model cooldown", errExec)
	}
	if cooldown.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d", cooldown.StatusCode())
	}
	retryAfter := cooldown.Headers().Get("Retry-After")
	if retryAfter != "60" && retryAfter != "59" && retryAfter != "58" {
		t.Fatalf("Retry-After = %q, want about 60", retryAfter)
	}
	if calls := exec.snapshot(); len(calls) != 2 {
		t.Fatalf("calls = %v, want one attempt per account", calls)
	}
}

func codexQuotaAuth(id string, remaining int) *Auth {
	return &Auth{
		ID:       id,
		Provider: "codex",
		Status:   StatusActive,
		Quota: QuotaState{
			Signals:    map[string]string{WeeklyQuotaRemainingPercentSignal: FormatWeeklyQuotaRemainingPercent(float64(remaining))},
			ObservedAt: time.Now(),
		},
	}
}
