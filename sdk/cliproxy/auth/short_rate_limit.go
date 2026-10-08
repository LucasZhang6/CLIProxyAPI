package auth

import (
	"errors"
	"strings"
	"time"
)

const (
	// shortRateLimitQuotaReason blocks every model on the credential without
	// entering the weekly quota ladder or rewriting weekly_quota_remaining_percent.
	shortRateLimitQuotaReason = "short_rate_limit"
	// shortWindowRateLimitCooldown is used when Codex omits Retry-After.
	// The observed bursts were a few seconds of "Rate limit exceeded".
	shortWindowRateLimitCooldown = time.Minute
)

func credentialWideQuotaReason(reason string) bool {
	return reason == "credential_quota" || reason == shortRateLimitQuotaReason
}

func shortWindowRateLimitRecovery(now time.Time, retryAfter *time.Duration) time.Time {
	cooldown := shortWindowRateLimitCooldown
	if retryAfter != nil && *retryAfter > 0 {
		cooldown = *retryAfter
		if cooldown < minQuotaCooldownFloor {
			cooldown = minQuotaCooldownFloor
		}
	}
	return now.Add(cooldown).Round(0)
}

func isCodexShortWindowRateLimit(auth *Auth, err *Error) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return shortWindowRateLimitError(err)
}

func shortWindowRateLimitError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != 429 {
		return false
	}
	text := strings.ToLower(err.Message + "\n" + err.Code)
	// usage_limit_reached is the weekly/plan window. "model rate limit" is one
	// model, and the sibling models on that account must stay selectable.
	if strings.Contains(text, "usage_limit_reached") || strings.Contains(text, "model rate limit") {
		return false
	}
	return strings.Contains(text, "rate limit exceeded") ||
		strings.Contains(text, "rate_limit_exceeded") ||
		strings.Contains(text, "rate_limit_error")
}

func isCodexShortWindowRateLimitError(err error) bool {
	return shortWindowRateLimitError(resultErrorFromError(err))
}

func markShortWindowRateLimit(auth *Auth, err *Error, retryAfter *time.Duration, now time.Time, disableCooling bool) bool {
	if disableCooling || !isCodexShortWindowRateLimit(auth, err) {
		return false
	}
	next := shortWindowRateLimitRecovery(now, retryAfter)
	// A longer plan-level cooldown already covers this account. Do not replace
	// it with the one-minute window, and do not climb the quota backoff ladder.
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(next) {
		return true
	}
	applyCodexShortWindowRateLimit(auth, next, now)
	return true
}

func applyCodexShortWindowRateLimit(auth *Auth, next, now time.Time) {
	if auth == nil {
		return
	}
	auth.Quota.Exceeded = true
	auth.Quota.Reason = shortRateLimitQuotaReason
	if auth.Quota.NextRecoverAt.Before(next) {
		auth.Quota.NextRecoverAt = next
	}
	auth.Unavailable = true
	auth.Status = StatusError
	auth.StatusMessage = "short rate limit"
	if auth.NextRetryAfter.Before(next) {
		auth.NextRetryAfter = next
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		state.Unavailable = true
		state.Status = StatusError
		state.UpdatedAt = now
		if state.NextRetryAfter.Before(next) {
			state.NextRetryAfter = next
		}
	}
}

// preferShortWindowCooldown keeps the upstream body for an ordinary failure.
// A Codex short-window 429 is different: once every account has been tried,
// the caller gets one 429 whose Retry-After is the earliest recovery time.
// The pick itself often reports "no auth available" because the tried set is
// already empty, so the cooldown has to be read back from the credentials.
func (m *Manager) preferShortWindowCooldown(lastErr, errPick error, providers []string, model string) error {
	if lastErr == nil || !isCodexShortWindowRateLimitError(lastErr) {
		if lastErr != nil {
			return lastErr
		}
		return errPick
	}
	var cooldown *modelCooldownError
	if errors.As(errPick, &cooldown) && cooldown != nil {
		return errPick
	}
	now := time.Now()
	var earliest time.Time
	if m != nil {
		m.mu.RLock()
		for _, candidate := range m.auths {
			if !codexShortWindowCandidate(candidate, providers, now) {
				continue
			}
			if earliest.IsZero() || candidate.Quota.NextRecoverAt.Before(earliest) {
				earliest = candidate.Quota.NextRecoverAt
			}
		}
		m.mu.RUnlock()
	}
	if earliest.IsZero() {
		return lastErr
	}
	resetIn := earliest.Sub(now)
	if resetIn < time.Second {
		resetIn = time.Second
	}
	return newModelCooldownError(model, "codex", resetIn)
}

func codexShortWindowCandidate(auth *Auth, providers []string, now time.Time) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if !auth.Quota.Exceeded || auth.Quota.Reason != shortRateLimitQuotaReason || !auth.Quota.NextRecoverAt.After(now) {
		return false
	}
	if len(providers) == 0 {
		return true
	}
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	for _, provider := range providers {
		if strings.ToLower(strings.TrimSpace(provider)) == providerKey {
			return true
		}
	}
	return false
}
