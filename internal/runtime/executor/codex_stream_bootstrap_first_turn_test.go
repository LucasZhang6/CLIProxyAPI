package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestIsCodexFirstTurnRequest(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"string input", `{"input":"hello"}`, true},
		{"instructions and user messages", `{"input":[{"type":"message","role":"developer","content":[]},{"role":"user","content":[]},{"type":"message","role":"user","content":[]}]}`, true},
		{"tool declarations", `{"input":[{"type":"additional_tools","tools":[]},{"role":"developer","content":[]},{"role":"user","content":[]}]}`, true},
		{"assistant reply", `{"input":[{"role":"user","content":[]},{"type":"message","role":"assistant","content":[]},{"role":"user","content":[]}]}`, false},
		{"tool output", `{"input":[{"role":"user","content":[]},{"type":"function_call","call_id":"c"},{"type":"function_call_output","call_id":"c"}]}`, false},
		{"custom tool output", `{"input":[{"role":"user","content":[]},{"type":"custom_tool_call_output","call_id":"c"}]}`, false},
		{"reasoning item", `{"input":[{"role":"user","content":[]},{"type":"reasoning","summary":[]}]}`, false},
		{"compaction summary", `{"input":[{"type":"compaction","encrypted_content":"x"},{"role":"user","content":[]}]}`, false},
		{"stored continuation", `{"previous_response_id":"resp_1","input":[{"role":"user","content":[]}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCodexFirstTurnRequest([]byte(tc.body)); got != tc.want {
				t.Fatalf("isCodexFirstTurnRequest() = %v, want %v", got, tc.want)
			}
		})
	}
}

func codexFirstTurnConfig(timeout, firstTurnTimeout, firstTurnWindow string) *config.Config {
	return &config.Config{Codex: config.CodexConfig{
		StreamBootstrapBuffering:        true,
		StreamBootstrapTimeout:          timeout,
		StreamBootstrapFirstTurnTimeout: firstTurnTimeout,
		StreamBootstrapFirstTurnWindow:  firstTurnWindow,
	}}
}

func TestCodexStreamBootstrapTimeoutFirstTurnExtension(t *testing.T) {
	now := time.Date(2026, 10, 14, 7, 0, 0, 0, time.UTC)
	firstTurn := []byte(`{"input":[{"role":"user","content":[]}]}`)
	laterTurn := []byte(`{"input":[{"role":"user","content":[]},{"role":"assistant","content":[]}]}`)
	startedAgo := func(d time.Duration) context.Context {
		return cliproxyexecutor.WithRequestStartedAt(context.Background(), now.Add(-d))
	}
	cases := []struct {
		name string
		ctx  context.Context
		cfg  *config.Config
		body []byte
		want time.Duration
	}{
		{"later turn keeps regular hold", startedAgo(0), codexFirstTurnConfig("40s", "60s", "90s"), laterTurn, 40 * time.Second},
		{"first turn without extension", startedAgo(0), codexFirstTurnConfig("40s", "", ""), firstTurn, 40 * time.Second},
		{"first turn extension", startedAgo(5 * time.Second), codexFirstTurnConfig("40s", "60s", "90s"), firstTurn, 60 * time.Second},
		{"window trims extension", startedAgo(45 * time.Second), codexFirstTurnConfig("40s", "60s", "90s"), firstTurn, 45 * time.Second},
		{"window never undercuts regular hold", startedAgo(80 * time.Second), codexFirstTurnConfig("40s", "60s", "90s"), firstTurn, 40 * time.Second},
		{"no request clock skips window", context.Background(), codexFirstTurnConfig("40s", "60s", "90s"), firstTurn, 60 * time.Second},
		{"shorter first-turn hold is ignored", startedAgo(0), codexFirstTurnConfig("40s", "30s", "90s"), firstTurn, 40 * time.Second},
		{"unlimited regular hold stays unlimited", startedAgo(0), codexFirstTurnConfig("", "60s", "90s"), firstTurn, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexStreamBootstrapTimeout(tc.ctx, tc.cfg, tc.body, now); got != tc.want {
				t.Fatalf("codexStreamBootstrapTimeout() = %v, want %v", got, tc.want)
			}
		})
	}
}

// lateOverloadServer sends the handshake, lets the mock clock pass `delay`, then rejects the
// request for capacity, matching upstream queueing that rejects after the regular hold.
func lateOverloadServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	clock := withMockClock(t, time.Date(2026, 10, 14, 7, 0, 0, 0, time.UTC))
	bootstrapStarted := make(chan struct{})
	var once sync.Once
	t.Cleanup(setCodexBootstrapNowForTest(func() time.Time {
		now := clock.now()
		once.Do(func() { close(bootstrapStarted) })
		return now
	}))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: " + codexCreatedEvent + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-bootstrapStarted
		clock.advance(delay)
		_, _ = w.Write([]byte("event: error\ndata: " + codexOverloadEvent + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

func TestCodexExecutor_FirstTurnLateOverloadFailsOver(t *testing.T) {
	server := lateOverloadServer(t, 11*time.Second)
	defer server.Close()

	req, opts := codexTestRequest()
	result, err := NewCodexExecutor(codexFirstTurnConfig("10s", "20s", "")).ExecuteStream(context.Background(), codexTestAuth(server.URL), req, opts)
	if err == nil || result != nil {
		t.Fatalf("result=%v err=%v, want the late first-turn overload to fail the attempt over", result, err)
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusServiceUnavailable {
		t.Fatalf("status code = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

func TestCodexExecutor_LaterTurnLateOverloadStaysInStream(t *testing.T) {
	server := lateOverloadServer(t, 11*time.Second)
	defer server.Close()

	req, opts := codexTestRequest()
	req.Payload = []byte(`{"model":"gpt-5.6-terra","input":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"again"}]}`)
	result, err := NewCodexExecutor(codexFirstTurnConfig("10s", "20s", "")).ExecuteStream(context.Background(), codexTestAuth(server.URL), req, opts)
	if err != nil || result == nil {
		t.Fatalf("result=%v err=%v, want a released stream for a later turn", result, err)
	}
	if _, streamErr := drainChunks(result); streamErr == nil {
		t.Fatal("expected the overload to be delivered in-stream")
	}
}

func TestCodexExecutor_FirstTurnWindowExhaustedStaysInStream(t *testing.T) {
	server := lateOverloadServer(t, 11*time.Second)
	defer server.Close()

	req, opts := codexTestRequest()
	ctx := cliproxyexecutor.WithRequestStartedAt(context.Background(), time.Date(2026, 10, 14, 7, 0, 0, 0, time.UTC).Add(-85*time.Second))
	result, err := NewCodexExecutor(codexFirstTurnConfig("10s", "20s", "90s")).ExecuteStream(ctx, codexTestAuth(server.URL), req, opts)
	if err != nil || result == nil {
		t.Fatalf("result=%v err=%v, want the regular hold once the first-turn window is spent", result, err)
	}
	if _, streamErr := drainChunks(result); streamErr == nil {
		t.Fatal("expected the overload to be delivered in-stream")
	}
}
