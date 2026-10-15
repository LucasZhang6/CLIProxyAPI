package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeCodexReasoningEffortForModel is a final outbound safety net for
// Codex requests. The generic thinking pipeline maps cross-provider high intent
// when it has authoritative selected-model capabilities, but real traffic can
// still arrive with a stale catalog, a user-defined route, or a payload override
// that leaves an unsupported effort in the final body. Clamp to the highest
// supported level not above the request so upstream does not reject the request
// with "level \"max\" not supported".
func normalizeCodexReasoningEffortForModel(body []byte, req cliproxyexecutor.Request, model string) []byte {
	effortResult := gjson.GetBytes(body, "reasoning.effort")
	if !effortResult.Exists() || effortResult.Type != gjson.String {
		return body
	}
	effort := strings.ToLower(strings.TrimSpace(effortResult.String()))
	if effort == "" {
		return body
	}
	levels := codexSupportedReasoningLevels(req, model)
	if len(levels) == 0 || reasoningLevelSupported(effort, levels) {
		return body
	}
	fallback := clampReasoningEffortToSupported(effort, levels)
	if fallback == "" || fallback == effort {
		return body
	}
	out, err := sjson.SetBytes(body, "reasoning.effort", fallback)
	if err != nil {
		return body
	}
	return out
}

func codexSupportedReasoningLevels(req cliproxyexecutor.Request, model string) []string {
	if info, ok := cliproxyauth.ResolvedModelInfo(req); ok && info != nil && info.Thinking != nil {
		return info.Thinking.Levels
	}
	if info := registry.LookupModelInfo(strings.TrimSpace(model), "codex"); info != nil && info.Thinking != nil {
		return info.Thinking.Levels
	}
	return nil
}

func reasoningLevelSupported(level string, levels []string) bool {
	for _, candidate := range levels {
		if strings.EqualFold(strings.TrimSpace(candidate), level) {
			return true
		}
	}
	return false
}

func clampReasoningEffortToSupported(effort string, levels []string) string {
	targetRank, ok := reasoningEffortRank(effort)
	if !ok {
		return ""
	}
	best := ""
	bestRank := -1
	lowest := ""
	lowestRank := 1 << 30
	for _, raw := range levels {
		level := strings.ToLower(strings.TrimSpace(raw))
		rank, okRank := reasoningEffortRank(level)
		if !okRank {
			continue
		}
		if rank < lowestRank {
			lowestRank = rank
			lowest = level
		}
		if rank <= targetRank && rank > bestRank {
			bestRank = rank
			best = level
		}
	}
	if best != "" {
		return best
	}
	return lowest
}

func reasoningEffortRank(level string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "none":
		return 0, true
	case "minimal":
		return 1, true
	case "low":
		return 2, true
	case "medium":
		return 3, true
	case "high":
		return 4, true
	case "xhigh":
		return 5, true
	case "max":
		return 6, true
	case "ultra":
		return 7, true
	default:
		return 0, false
	}
}
