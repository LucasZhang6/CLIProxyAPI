package auth

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// WeeklyQuotaRemainingPercentSignal is the credential-level weekly remaining
// percent read first by the quota-aware selector. Probes and the management
// quota refresh write it; response-header observation keeps it when the new
// snapshot has no seven-day window.
const WeeklyQuotaRemainingPercentSignal = "weekly_quota_remaining_percent"

// Codex weekly windows are seven days. HTTP quota headers report that span in
// minutes; the wham/usage JSON reports it in seconds (604800).
const codexWeeklyWindowMinutes = 10080

// FormatWeeklyQuotaRemainingPercent renders a remaining percent the selector
// already accepts, clamped to 0–100 and rounded to the nearest integer.
func FormatWeeklyQuotaRemainingPercent(remaining float64) string {
	remaining = clampPercent(remaining)
	return fmt.Sprintf("%d%%", int(math.Round(remaining)))
}

// SetWeeklyQuotaRemainingPercent writes the weekly remaining percent onto the
// live credential the selector clones at pick time. It does not persist the
// auth file.
func (m *Manager) SetWeeklyQuotaRemainingPercent(authID string, remaining float64) bool {
	if m == nil || math.IsNaN(remaining) || math.IsInf(remaining, 0) {
		return false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	formatted := FormatWeeklyQuotaRemainingPercent(remaining)

	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.auths[authID]
	if current == nil || !strings.EqualFold(strings.TrimSpace(current.Provider), "codex") {
		return false
	}
	if current.Quota.Signals == nil {
		current.Quota.Signals = make(map[string]string, 1)
	}
	current.Quota.Signals[WeeklyQuotaRemainingPercentSignal] = formatted
	current.Quota.ObservedAt = time.Now()
	return true
}

// applyCodexWeeklyQuotaSignal copies a probed weekly remaining into the new
// snapshot, or replaces it when this response itself carries a seven-day window.
func applyCodexWeeklyQuotaSignal(previous, next map[string]string) {
	if next == nil {
		return
	}
	if remaining, ok := weeklyRemainingFromCodexWindowHeaders(next); ok {
		next[WeeklyQuotaRemainingPercentSignal] = FormatWeeklyQuotaRemainingPercent(remaining)
		return
	}
	if previous == nil {
		return
	}
	value, ok := signalValueCaseInsensitive(previous, WeeklyQuotaRemainingPercentSignal)
	if !ok || !validQuotaSignalValue(value) {
		return
	}
	next[WeeklyQuotaRemainingPercentSignal] = value
}

func weeklyRemainingFromCodexWindowHeaders(signals map[string]string) (float64, bool) {
	for _, window := range []string{"Primary", "Secondary"} {
		minutes, okMinutes := signalValueCaseInsensitive(signals, "X-Codex-"+window+"-Window-Minutes")
		used, okUsed := signalValueCaseInsensitive(signals, "X-Codex-"+window+"-Used-Percent")
		if !okMinutes || !okUsed || !codexHeaderWindowIsWeekly(minutes) {
			continue
		}
		usedPercent, okParse := parsePercentSignal(used)
		if !okParse {
			continue
		}
		return clampPercent(100 - usedPercent), true
	}
	return 0, false
}

func codexHeaderWindowIsWeekly(minutes string) bool {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(minutes), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return false
	}
	return parsed == codexWeeklyWindowMinutes
}
