package executor

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func shouldBufferCodexStreamBootstrap(cfg *config.Config, from sdktranslator.Format) bool {
	return cfg != nil && cfg.Codex.StreamBootstrapBuffering && from != sdktranslator.FormatClaude
}

// codexStreamBootstrapTimeout returns how long one attempt may hold downstream headers,
// counted from bootstrapStart. Conversation-opening requests may hold longer, but never past
// the first-turn window measured from the request start, and never shorter than the regular
// timeout.
func codexStreamBootstrapTimeout(ctx context.Context, cfg *config.Config, body []byte, bootstrapStart time.Time) time.Duration {
	if cfg == nil {
		return 0
	}
	base := cfg.Codex.StreamBootstrapTimeoutDuration()
	hold := cfg.Codex.StreamBootstrapFirstTurnTimeoutDuration()
	if base <= 0 || hold <= base || !isCodexFirstTurnRequest(body) {
		return base
	}
	if window := cfg.Codex.StreamBootstrapFirstTurnWindowDuration(); window > 0 {
		if startedAt, ok := cliproxyexecutor.RequestStartedAt(ctx); ok {
			hold = min(hold, window-bootstrapStart.Sub(startedAt))
		}
	}
	return max(hold, base)
}

// isCodexFirstTurnRequest reports whether a Responses request opens a conversation: it does
// not continue a stored response and its input replays no model output from earlier turns.
func isCodexFirstTurnRequest(body []byte) bool {
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		return false
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return true
	}
	firstTurn := true
	input.ForEach(func(_, item gjson.Result) bool {
		if isCodexModelOutputItem(item) {
			firstTurn = false
			return false
		}
		return true
	})
	return firstTurn
}

// isCodexModelOutputItem reports whether an input item replays an earlier turn: an assistant
// message, reasoning, a compaction summary, or a tool call and its result. Declarations such
// as additional_tools do not count.
func isCodexModelOutputItem(item gjson.Result) bool {
	if item.Get("role").String() == "assistant" {
		return true
	}
	itemType := item.Get("type").String()
	switch itemType {
	case "reasoning", "compaction", "item_reference":
		return true
	}
	return strings.HasSuffix(itemType, "_call") || strings.HasSuffix(itemType, "_call_output")
}
