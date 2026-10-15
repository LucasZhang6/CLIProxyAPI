package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexBootstrapPolicyBypassesClaudeMessages(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{StreamBootstrapBuffering: true}}
	if shouldBufferCodexStreamBootstrap(cfg, sdktranslator.FormatClaude) {
		t.Fatal("Claude /v1/messages must bypass Codex bootstrap buffering")
	}
	if !shouldBufferCodexStreamBootstrap(cfg, sdktranslator.FormatOpenAIResponse) {
		t.Fatal("native Responses requests should retain bootstrap buffering")
	}
}

func TestCodexExecutorBootstrapTimeoutReleasesWithoutAnotherFrame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.in_progress\ndata: " + codexInProgressEvent + "\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, opts := codexTestRequest()
	started := time.Now()
	result, err := NewCodexExecutor(codexBufferingConfigWithTimeout(true, "50ms")).ExecuteStream(ctx, codexTestAuth(server.URL), req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if result == nil {
		t.Fatal("expected stream result after wall-clock bootstrap timeout")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("bootstrap release took %s, want under 500ms without another upstream frame", elapsed)
	}
	cancel()
}

func TestCodexWebsocketBootstrapTimeoutReleasesWithoutAnotherFrame(t *testing.T) {
	hold := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		if err = conn.WriteMessage(websocket.TextMessage, []byte(codexInProgressEvent)); err != nil {
			return
		}
		<-hold
	}))
	defer server.Close()
	defer close(hold)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, opts := codexWebsocketRequest()
	started := time.Now()
	result, err := NewCodexWebsocketsExecutor(codexBufferingConfigWithTimeout(true, "50ms")).ExecuteStream(ctx, codexTestAuth(server.URL), req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if result == nil {
		t.Fatal("expected websocket stream result after wall-clock bootstrap timeout")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("websocket bootstrap release took %s, want under 500ms without another upstream frame", elapsed)
	}
	cancel()
}
