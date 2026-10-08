package helps

import (
	"math"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// CodexWeeklyLimitWindowSeconds is the ChatGPT wham/usage span for the weekly
// bucket. Five-hour windows are 18000 seconds and are not weekly quota.
const CodexWeeklyLimitWindowSeconds int64 = 604800

// CodexWeeklyRemainingPercent returns the weekly remaining percent
// (100 - used_percent) from a ChatGPT /backend-api/wham/usage body.
//
// The weekly window is the primary or secondary window whose
// limit_window_seconds is 604800, matching the management quota card.
// A five-hour window and a 28–31 day window are ignored. ok is false when
// the body has no weekly window with a usable used percent.
func CodexWeeklyRemainingPercent(body []byte) (float64, bool) {
	if len(body) == 0 {
		return 0, false
	}
	root := gjson.ParseBytes(body)
	rateLimit := firstCodexQuotaResult(root, "rate_limit", "rateLimit")
	if !rateLimit.Exists() || !rateLimit.IsObject() {
		return 0, false
	}
	for _, names := range [][2]string{
		{"primary_window", "primaryWindow"},
		{"secondary_window", "secondaryWindow"},
	} {
		window := firstCodexQuotaResult(rateLimit, names[0], names[1])
		if !window.Exists() || !window.IsObject() {
			continue
		}
		seconds, okSeconds := codexLimitWindowSeconds(window)
		if !okSeconds || seconds != CodexWeeklyLimitWindowSeconds {
			continue
		}
		used, okUsed := codexWindowUsedPercent(window, rateLimit)
		if !okUsed {
			continue
		}
		return clampCodexPercent(100 - used), true
	}
	return 0, false
}

func codexLimitWindowSeconds(window gjson.Result) (int64, bool) {
	value := firstCodexQuotaResult(window, "limit_window_seconds", "limitWindowSeconds")
	if !value.Exists() {
		return 0, false
	}
	switch value.Type {
	case gjson.Number:
		return value.Int(), true
	case gjson.String:
		parsed, err := strconv.ParseInt(strings.TrimSpace(value.String()), 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func codexWindowUsedPercent(window, rateLimit gjson.Result) (float64, bool) {
	used := firstCodexQuotaResult(window, "used_percent", "usedPercent")
	if used.Exists() {
		switch used.Type {
		case gjson.Number:
			value := used.Float()
			if !math.IsNaN(value) && !math.IsInf(value, 0) {
				return value, true
			}
		case gjson.String:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(used.String()), "%")), 64)
			if err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
				return parsed, true
			}
		}
	}
	if !codexWindowHasReset(window) {
		return 0, false
	}
	limitReached := firstCodexQuotaResult(rateLimit, "limit_reached", "limitReached")
	allowed := firstCodexQuotaResult(rateLimit, "allowed")
	if (limitReached.Exists() && limitReached.Bool()) || (allowed.Exists() && allowed.Type == gjson.False) {
		return 100, true
	}
	return 0, false
}

func codexWindowHasReset(window gjson.Result) bool {
	resetAt := firstCodexQuotaResult(window, "reset_at", "resetAt")
	if resetAt.Exists() && resetAt.Float() > 0 {
		return true
	}
	resetAfter := firstCodexQuotaResult(window, "reset_after_seconds", "resetAfterSeconds")
	return resetAfter.Exists() && resetAfter.Float() > 0
}

func clampCodexPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
