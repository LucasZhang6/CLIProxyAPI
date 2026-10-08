package management

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	codexWhamUsageURL       = "https://chatgpt.com/backend-api/wham/usage"
	codexQuotaProbeInterval = time.Hour
	codexQuotaProbeTimeout  = 30 * time.Second
	codexQuotaProbeMaxBody  = 1 << 20
	// Same client identity the management quota card sends to wham/usage.
	codexQuotaProbeUserAgent = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
)

// StartCodexQuotaProbe refreshes weekly remaining for every enabled Codex
// credential once at startup and then once an hour. Weekly quota moves slowly,
// so a denser interval does not change scheduling.
func (h *Handler) StartCodexQuotaProbe(ctx context.Context) {
	if h == nil || h.authManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.codexQuotaProbeOnce.Do(func() {
		go h.loopCodexQuotaProbe(ctx)
	})
}

func (h *Handler) loopCodexQuotaProbe(ctx context.Context) {
	h.probeEnabledCodexWeeklyQuotas(ctx)
	ticker := time.NewTicker(codexQuotaProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.probeEnabledCodexWeeklyQuotas(ctx)
		}
	}
}

func (h *Handler) probeEnabledCodexWeeklyQuotas(ctx context.Context) {
	if h == nil || h.authManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.codexQuotaProbeMu.Lock()
	if h.codexQuotaProbeRunning {
		h.codexQuotaProbeMu.Unlock()
		return
	}
	h.codexQuotaProbeRunning = true
	h.codexQuotaProbeMu.Unlock()
	defer func() {
		h.codexQuotaProbeMu.Lock()
		h.codexQuotaProbeRunning = false
		h.codexQuotaProbeMu.Unlock()
	}()

	for _, auth := range h.authManager.List() {
		if ctx.Err() != nil {
			return
		}
		if !codexQuotaProbeEligible(auth) {
			continue
		}
		h.probeCodexWeeklyQuota(ctx, auth)
	}
}

func codexQuotaProbeEligible(auth *coreauth.Auth) bool {
	if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return codexProbeAccessToken(auth) != ""
}

func (h *Handler) probeCodexWeeklyQuota(ctx context.Context, auth *coreauth.Auth) {
	token := codexProbeAccessToken(auth)
	if token == "" || auth == nil {
		return
	}
	label := codexQuotaProbeLabel(auth)
	reqCtx, cancel := context.WithTimeout(ctx, codexQuotaProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, codexWhamUsageURL, nil)
	if err != nil {
		log.Warnf("codex weekly quota probe failed for %s: %v", label, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", codexQuotaProbeUserAgent)
	if accountID := codexProbeAccountID(auth); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}

	resp, err := h.doCodexQuotaProbe(req, auth)
	if err != nil {
		log.Warnf("codex weekly quota probe failed for %s: %v", label, err)
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("codex weekly quota probe body close: %v", errClose)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, codexQuotaProbeMaxBody))
	if err != nil {
		log.Warnf("codex weekly quota probe failed for %s: %v", label, err)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warnf("codex weekly quota probe failed for %s: status %d", label, resp.StatusCode)
		return
	}
	remaining, ok := helps.CodexWeeklyRemainingPercent(body)
	if !ok {
		log.Warnf("codex weekly quota probe for %s returned no weekly window", label)
		return
	}
	if h.authManager.SetWeeklyQuotaRemainingPercent(auth.ID, remaining) {
		log.Infof("codex weekly quota: %s remaining %s", label, coreauth.FormatWeeklyQuotaRemainingPercent(remaining))
	}
}

func (h *Handler) doCodexQuotaProbe(req *http.Request, auth *coreauth.Auth) (*http.Response, error) {
	if h != nil && h.codexQuotaProbeDo != nil {
		return h.codexQuotaProbeDo(req)
	}
	client := &http.Client{Timeout: codexQuotaProbeTimeout}
	if h != nil {
		client.Transport = h.apiCallTransport(auth, "")
	}
	return client.Do(req)
}

// noteCodexWhamUsage records the weekly remaining percent when the management
// quota card refreshes /backend-api/wham/usage. The card and the hourly probe
// write the same selector signal.
func (h *Handler) noteCodexWhamUsage(auth *coreauth.Auth, requestURL *url.URL, statusCode int, body []byte) {
	if h == nil || h.authManager == nil || auth == nil || statusCode < 200 || statusCode >= 300 {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}
	if !isCodexWhamUsageURL(requestURL) {
		return
	}
	remaining, ok := helps.CodexWeeklyRemainingPercent(body)
	if !ok {
		return
	}
	h.authManager.SetWeeklyQuotaRemainingPercent(auth.ID, remaining)
}

func isCodexWhamUsageURL(requestURL *url.URL) bool {
	if requestURL == nil {
		return false
	}
	switch strings.ToLower(requestURL.Scheme) {
	case "https", "http":
	default:
		return false
	}
	host := strings.ToLower(requestURL.Hostname())
	if host != "chatgpt.com" && host != "www.chatgpt.com" {
		return false
	}
	return requestURL.Path == "/backend-api/wham/usage"
}

func codexProbeAccessToken(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	token, _ := auth.Metadata["access_token"].(string)
	return strings.TrimSpace(token)
}

func codexProbeAccountID(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	accountID, _ := auth.Metadata["account_id"].(string)
	return strings.TrimSpace(accountID)
}

func codexQuotaProbeLabel(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if email, ok := auth.Metadata["email"].(string); ok && strings.TrimSpace(email) != "" {
			return strings.TrimSpace(email)
		}
	}
	if auth.Attributes != nil {
		if email := strings.TrimSpace(auth.Attributes["email"]); email != "" {
			return email
		}
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	return auth.ID
}
