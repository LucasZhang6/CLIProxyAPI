package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

const delayedClaudeStreamModel = "delayed-claude-stream-model"

type delayedClaudeStreamExecutor struct {
	delay    time.Duration
	started  chan struct{}
	canceled chan struct{}
}

func (*delayedClaudeStreamExecutor) Identifier() string { return "delayed-claude-stream-executor" }

func (*delayedClaudeStreamExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *delayedClaudeStreamExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	select {
	case e.started <- struct{}{}:
	default:
	}
	select {
	case <-time.After(e.delay):
		chunks := make(chan coreexecutor.StreamChunk)
		close(chunks)
		return &coreexecutor.StreamResult{Chunks: chunks}, nil
	case <-ctx.Done():
		select {
		case e.canceled <- struct{}{}:
		default:
		}
		return nil, ctx.Err()
	}
}

func (*delayedClaudeStreamExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (*delayedClaudeStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*delayedClaudeStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newDelayedClaudeHandler(t *testing.T, executor *delayedClaudeStreamExecutor, keepAliveSeconds int) *gin.Engine {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: t.Name(), Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: delayedClaudeStreamModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	cfg := &sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: keepAliveSeconds}}
	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager))
	router := gin.New()
	router.POST("/v1/messages", handler.ClaudeMessages)
	return router
}

func TestClaudeErrorExtractsOpenAIStyleUpstreamJSON(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}

	got := handler.toClaudeError(msg)

	if got.Type != "error" {
		t.Fatalf("type = %q, want error", got.Type)
	}
	if got.Error.Type != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error", got.Error.Type)
	}
	if got.Error.Message != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestClaudeErrorExtractsClaudeStyleUpstreamJSON(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."},"request_id":"req_123"}`),
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error", got.Error.Type)
	}
	if got.Error.Message != "This request would exceed your account's rate limit. Please try again later." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestWriteClaudeErrorResponseUsesClaudeEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}

	handler.WriteErrorResponse(c, msg)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	body := recorder.Body.Bytes()
	if got := gjson.GetBytes(body, "type").String(); got != "error" {
		t.Fatalf("type = %q, want error; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("error.message = %q; body=%s", got, body)
	}
}

func TestWriteClaudeErrorResponse_IncludesRetryAfterForModelCooldownDefaultSettings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}

	cooldownErr := coreauth.NewManager(nil, nil, nil)
	_ = cooldownErr
	// Create mock model cooldown error
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      coreauth.NewModelCooldownError("claude-sonnet-4-6", "claude", 20*time.Second),
	}

	handler.WriteErrorResponse(c, msg)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "20" {
		t.Fatalf("Retry-After = %q, want 20", got)
	}
}

func TestPendingClaudeStreamErrorUsesBufferedError(t *testing.T) {
	wantErr := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- wantErr
	close(errs)

	gotErr, ok := handlers.PendingStreamError(errs)
	if !ok {
		t.Fatal("expected pending stream error")
	}
	if gotErr != wantErr {
		t.Fatalf("pending error = %p, want %p", gotErr, wantErr)
	}
}

func TestClaudeStreamKeepAliveDefault(t *testing.T) {
	if defaultClaudeStreamKeepAliveInterval != 15*time.Second {
		t.Fatalf("default keep-alive = %s, want 15s", defaultClaudeStreamKeepAliveInterval)
	}
}

func TestClaudeStreamFlushesKeepAliveWhileUpstreamStarts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &delayedClaudeStreamExecutor{
		delay:    1200 * time.Millisecond,
		started:  make(chan struct{}, 1),
		canceled: make(chan struct{}, 1),
	}
	router := newDelayedClaudeHandler(t, executor, 1)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+delayedClaudeStreamModel+`","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if !strings.Contains(recorder.Body.String(), ": keep-alive\n\n") {
		t.Fatalf("response did not contain pre-stream keep-alive: %q", recorder.Body.String())
	}
}

func TestClaudeStreamClientDisconnectCancelsUpstreamStart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &delayedClaudeStreamExecutor{
		delay:    10 * time.Second,
		started:  make(chan struct{}, 1),
		canceled: make(chan struct{}, 1),
	}
	router := newDelayedClaudeHandler(t, executor, 1)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+delayedClaudeStreamModel+`","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(requestCtx)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(recorder, request)
		close(done)
	}()

	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("upstream execution did not start")
	}
	cancelRequest()
	select {
	case <-executor.canceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("upstream context was not canceled promptly")
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handler did not return promptly after client disconnect")
	}
}
