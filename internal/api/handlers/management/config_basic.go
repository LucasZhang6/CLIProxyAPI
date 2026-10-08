package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	defaultLatestReleaseURL = "https://api.github.com/repos/router-for-me/CLIProxyAPI/releases/latest"
	latestReleaseURL        = defaultLatestReleaseURL
	latestReleaseUserAgent  = "CLIProxyAPI"
	latestVersionCacheTTL   = 15 * time.Minute
)

type latestVersionCache struct {
	mu         sync.Mutex
	version    string
	fetchedAt  time.Time
	inflight   *latestVersionLookup
	refreshing bool
	endpoint   string
	nowFunc    func() time.Time
}

type latestVersionLookup struct {
	done    chan struct{}
	version string
	err     error
}

func (h *Handler) GetConfig(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{})
		return
	}
	if isUserAccount(c) {
		data, err := json.Marshal(h.cfg)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encode_failed"})
			return
		}
		var encoded map[string]any
		if err = json.Unmarshal(data, &encoded); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encode_failed"})
			return
		}
		c.JSON(200, accountSanitizeJSONConfig(encoded))
		return
	}
	c.JSON(200, new(*h.cfg))
}

type releaseInfo struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
}

func setLatestReleaseRequestHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", latestReleaseUserAgent)
	if token := util.ResolveGitHubToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// GetLatestVersion returns the latest release version from GitHub without downloading assets.
// Fresh cache hits return immediately. Stale values are served while a background refresh runs.
func (h *Handler) GetLatestVersion(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unavailable"})
		return
	}
	proxyURL := ""
	if h.cfg != nil {
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
	}
	if version, stale := h.latestVersion.cached(); version != "" {
		c.JSON(http.StatusOK, gin.H{"latest-version": version})
		if stale {
			h.latestVersion.refreshAsync(proxyURL, h.fetchLatestVersion)
		}
		return
	}
	version, err := h.latestVersion.fetchShared(c.Request.Context(), proxyURL, h.fetchLatestVersion)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "request_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"latest-version": version})
}

func (cache *latestVersionCache) now() time.Time {
	if cache != nil && cache.nowFunc != nil {
		return cache.nowFunc()
	}
	return time.Now()
}

func (cache *latestVersionCache) endpointURL() string {
	if cache != nil && strings.TrimSpace(cache.endpoint) != "" {
		return strings.TrimSpace(cache.endpoint)
	}
	return defaultLatestReleaseURL
}

func (cache *latestVersionCache) cached() (string, bool) {
	if cache == nil {
		return "", false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.version == "" {
		return "", false
	}
	return cache.version, cache.now().Sub(cache.fetchedAt) >= latestVersionCacheTTL
}

func (cache *latestVersionCache) fetchShared(ctx context.Context, proxyURL string, fetch func(context.Context, string) (string, error)) (string, error) {
	if cache == nil {
		return "", fmt.Errorf("latest version cache is nil")
	}
	cache.mu.Lock()
	if cache.version != "" && cache.now().Sub(cache.fetchedAt) < latestVersionCacheTTL {
		version := cache.version
		cache.mu.Unlock()
		return version, nil
	}
	if pending := cache.inflight; pending != nil {
		cache.mu.Unlock()
		select {
		case <-pending.done:
			return pending.version, pending.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	lookup := &latestVersionLookup{done: make(chan struct{})}
	cache.inflight = lookup
	cache.mu.Unlock()

	version, err := fetch(ctx, proxyURL)
	cache.mu.Lock()
	if err == nil && strings.TrimSpace(version) != "" {
		cache.version = strings.TrimSpace(version)
		cache.fetchedAt = cache.now()
	}
	lookup.version = cache.version
	lookup.err = err
	cache.inflight = nil
	close(lookup.done)
	cache.mu.Unlock()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(version), nil
}

func (cache *latestVersionCache) refreshAsync(proxyURL string, fetch func(context.Context, string) (string, error)) {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	if cache.refreshing || cache.inflight != nil {
		cache.mu.Unlock()
		return
	}
	cache.refreshing = true
	cache.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = cache.fetchShared(ctx, proxyURL, fetch)
		cache.mu.Lock()
		cache.refreshing = false
		cache.mu.Unlock()
	}()
}

func (h *Handler) fetchLatestVersion(ctx context.Context, proxyURL string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	if strings.TrimSpace(proxyURL) != "" {
		sdkCfg := &sdkconfig.SDKConfig{ProxyURL: proxyURL}
		util.SetProxy(sdkCfg, client)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.latestVersion.endpointURL(), nil)
	if err != nil {
		return "", err
	}
	setLatestReleaseRequestHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close latest version response body")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var info releaseInfo
	if errDecode := json.NewDecoder(resp.Body).Decode(&info); errDecode != nil {
		return "", errDecode
	}
	version := strings.TrimSpace(info.TagName)
	if version == "" {
		version = strings.TrimSpace(info.Name)
	}
	if version == "" {
		return "", fmt.Errorf("missing release version")
	}
	return version, nil
}

func WriteConfig(path string, data []byte) error {
	data = config.NormalizeCommentIndentation(data)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, errWrite := f.Write(data); errWrite != nil {
		_ = f.Close()
		return errWrite
	}
	if errSync := f.Sync(); errSync != nil {
		_ = f.Close()
		return errSync
	}
	return f.Close()
}

func (h *Handler) PutConfigYAML(c *gin.Context) {
	accountUser := isUserAccount(c)
	var body []byte
	var err error
	if accountUser {
		body, err = io.ReadAll(io.LimitReader(c.Request.Body, maxAccountConfigYAMLBodyBytes+1))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": "cannot read request body"})
			return
		}
		if int64(len(body)) > maxAccountConfigYAMLBodyBytes {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_too_large"})
			return
		}
	} else {
		body, err = io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": "cannot read request body"})
			return
		}
	}
	if !h.validateAndWriteConfigYAML(c, body, accountUser) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "changed": []string{"config"}})
}

func (h *Handler) accountConfigYAMLBody(c *gin.Context, body []byte) ([]byte, error) {
	originalBody, err := os.ReadFile(h.configFilePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed"})
		return nil, err
	}
	originalDoc, err := decodeAccountConfigYAML(originalBody)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid_config"})
		return nil, err
	}
	requestedDoc, err := decodeAccountConfigYAML(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml"})
		return nil, err
	}
	mergedDoc, err := mergeAccountConfigYAMLWithProtectedOriginal(originalDoc, requestedDoc)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "protected_config"})
		return nil, err
	}
	mergedBody, err := encodeAccountConfigYAML(mergedDoc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encode_failed"})
		return nil, err
	}
	return mergedBody, nil
}

func (h *Handler) validateAndWriteConfigYAML(c *gin.Context, body []byte, genericErrors bool) bool {
	// Serialize protected-field merging with all other persisted config changes.
	// Otherwise an operator could restore a stale billing/admin snapshot.
	h.mu.Lock()
	defer h.mu.Unlock()
	if genericErrors {
		merged, err := h.accountConfigYAMLBody(c, body)
		if err != nil {
			return false
		}
		body = merged
	}
	var cfg config.Config
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		if genericErrors {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": err.Error()})
		}
		return false
	}
	// Validate config using LoadConfigOptional with optional=false to enforce parsing
	tmpDir := filepath.Dir(h.configFilePath)
	tmpFile, err := os.CreateTemp(tmpDir, "config-validate-*.yaml")
	if err != nil {
		if genericErrors {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": err.Error()})
		}
		return false
	}
	tempFile := tmpFile.Name()
	if _, errWrite := tmpFile.Write(body); errWrite != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tempFile)
		if genericErrors {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": errWrite.Error()})
		}
		return false
	}
	if errClose := tmpFile.Close(); errClose != nil {
		_ = os.Remove(tempFile)
		if genericErrors {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": errClose.Error()})
		}
		return false
	}
	defer func() {
		_ = os.Remove(tempFile)
	}()
	_, err = config.LoadConfigOptional(tempFile, false)
	if err != nil {
		if genericErrors {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config"})
		} else {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
		}
		return false
	}
	if WriteConfig(h.configFilePath, body) != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": "failed to write config"})
		return false
	}
	// Reload into handler to keep memory in sync
	newCfg, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		if genericErrors {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "reload_failed"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "reload_failed", "message": err.Error()})
		}
		return false
	}
	h.cfg = newCfg
	return true
}

// GetConfigYAML returns config.yaml bytes. Admins receive the raw file with
// formatting/comments preserved; account users receive re-encoded sanitized YAML.
func (h *Handler) GetConfigYAML(c *gin.Context) {
	data, err := os.ReadFile(h.configFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "config file not found"})
			return
		}
		if isUserAccount(c) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	if isUserAccount(c) {
		doc, err := decodeAccountConfigYAML(data)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid_config"})
			return
		}
		data, err = encodeAccountConfigYAML(sanitizeAccountConfigYAMLDocument(doc))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encode_failed"})
			return
		}
	}
	c.Header("Content-Type", "application/yaml; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	_, _ = c.Writer.Write(data)
}

// Debug
func (h *Handler) GetDebug(c *gin.Context) { c.JSON(200, gin.H{"debug": h.cfg.Debug}) }
func (h *Handler) PutDebug(c *gin.Context) { h.updateBoolField(c, func(v bool) { h.cfg.Debug = v }) }

// UsageStatisticsEnabled
func (h *Handler) GetUsageStatisticsEnabled(c *gin.Context) {
	c.JSON(200, gin.H{"usage-statistics-enabled": h.cfg.UsageStatisticsEnabled})
}
func (h *Handler) PutUsageStatisticsEnabled(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.UsageStatisticsEnabled = v })
}

// UsageStatisticsEnabled
func (h *Handler) GetLoggingToFile(c *gin.Context) {
	c.JSON(200, gin.H{"logging-to-file": h.cfg.LoggingToFile})
}
func (h *Handler) PutLoggingToFile(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.LoggingToFile = v })
}

// LogsMaxTotalSizeMB
func (h *Handler) GetLogsMaxTotalSizeMB(c *gin.Context) {
	c.JSON(200, gin.H{"logs-max-total-size-mb": h.cfg.LogsMaxTotalSizeMB})
}
func (h *Handler) PutLogsMaxTotalSizeMB(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := *body.Value
	if value < 0 {
		value = 0
	}
	h.cfg.LogsMaxTotalSizeMB = value
	h.persist(c)
}

// ErrorLogsMaxFiles
func (h *Handler) GetErrorLogsMaxFiles(c *gin.Context) {
	c.JSON(200, gin.H{"error-logs-max-files": h.cfg.ErrorLogsMaxFiles})
}
func (h *Handler) PutErrorLogsMaxFiles(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := *body.Value
	if value < 0 {
		value = 10
	}
	h.cfg.ErrorLogsMaxFiles = value
	h.persist(c)
}

// Request log
func (h *Handler) GetRequestLog(c *gin.Context) { c.JSON(200, gin.H{"request-log": h.cfg.RequestLog}) }
func (h *Handler) PutRequestLog(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.RequestLog = v })
}

// Websocket auth
func (h *Handler) GetWebsocketAuth(c *gin.Context) {
	c.JSON(200, gin.H{"ws-auth": h.cfg.WebsocketAuth})
}
func (h *Handler) PutWebsocketAuth(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.WebsocketAuth = v })
}

// Request retry
func (h *Handler) GetRequestRetry(c *gin.Context) {
	c.JSON(200, gin.H{"request-retry": h.cfg.RequestRetry})
}
func (h *Handler) PutRequestRetry(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.RequestRetry = v })
}

// Max retry credentials
func (h *Handler) GetMaxRetryCredentials(c *gin.Context) {
	c.JSON(200, gin.H{"max-retry-credentials": h.cfg.MaxRetryCredentials})
}
func (h *Handler) PutMaxRetryCredentials(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.MaxRetryCredentials = v })
}

// Max retry interval
func (h *Handler) GetMaxRetryInterval(c *gin.Context) {
	c.JSON(200, gin.H{"max-retry-interval": h.cfg.MaxRetryInterval})
}
func (h *Handler) PutMaxRetryInterval(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.MaxRetryInterval = v })
}

// ForceModelPrefix
func (h *Handler) GetForceModelPrefix(c *gin.Context) {
	c.JSON(200, gin.H{"force-model-prefix": h.cfg.ForceModelPrefix})
}
func (h *Handler) PutForceModelPrefix(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.ForceModelPrefix = v })
}

func normalizeRoutingStrategy(strategy string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(strategy))
	switch normalized {
	case "", "round-robin", "roundrobin", "rr":
		return "round-robin", true
	case "weighted-round-robin", "weightedroundrobin", "wrr":
		return "weighted-round-robin", true
	case "fill-first", "fillfirst", "ff":
		return "fill-first", true
	case "quota-aware", "quotaaware", "qa":
		return "quota-aware", true
	default:
		return "", false
	}
}

// RoutingStrategy
func (h *Handler) GetRoutingStrategy(c *gin.Context) {
	strategy, ok := normalizeRoutingStrategy(h.cfg.Routing.Strategy)
	if !ok {
		c.JSON(200, gin.H{"strategy": strings.TrimSpace(h.cfg.Routing.Strategy)})
		return
	}
	c.JSON(200, gin.H{"strategy": strategy})
}
func (h *Handler) PutRoutingStrategy(c *gin.Context) {
	var body struct {
		Value *string `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	normalized, ok := normalizeRoutingStrategy(*body.Value)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid strategy"})
		return
	}
	if isUserAccount(c) {
		current, _ := normalizeRoutingStrategy(h.cfg.Routing.Strategy)
		if current == "quota-aware" || normalized == "quota-aware" {
			c.JSON(http.StatusForbidden, gin.H{"error": "protected_config"})
			return
		}
	}
	h.cfg.Routing.Strategy = normalized
	h.persist(c)
}

func quotaAwareWeeklyRemainingMinPercent(cfg *config.Config) int {
	if cfg == nil || cfg.Routing.QuotaAware.WeeklyRemainingMinPercent == nil {
		return 20
	}
	value := *cfg.Routing.QuotaAware.WeeklyRemainingMinPercent
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func (h *Handler) GetRoutingQuotaAware(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"strategy":                     strings.TrimSpace(h.cfg.Routing.Strategy),
		"enabled":                      strings.EqualFold(strings.TrimSpace(h.cfg.Routing.Strategy), "quota-aware"),
		"weekly_remaining_min_percent": quotaAwareWeeklyRemainingMinPercent(h.cfg),
	})
}

func (h *Handler) PutRoutingQuotaAware(c *gin.Context) {
	var body struct {
		Strategy                      *string `json:"strategy"`
		Enabled                       *bool   `json:"enabled"`
		WeeklyRemainingMinPercent     *int    `json:"weekly_remaining_min_percent"`
		WeeklyRemainingMinPercentYAML *int    `json:"weekly-remaining-min-percent"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if isUserAccount(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "protected_config"})
		return
	}
	if body.Strategy != nil {
		normalized, ok := normalizeRoutingStrategy(*body.Strategy)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid strategy"})
			return
		}
		h.cfg.Routing.Strategy = normalized
	}
	if body.Enabled != nil {
		if *body.Enabled {
			h.cfg.Routing.Strategy = "quota-aware"
		} else if strings.EqualFold(strings.TrimSpace(h.cfg.Routing.Strategy), "quota-aware") {
			h.cfg.Routing.Strategy = "round-robin"
		}
	}
	threshold := body.WeeklyRemainingMinPercent
	if threshold == nil {
		threshold = body.WeeklyRemainingMinPercentYAML
	}
	if threshold != nil {
		value := *threshold
		if value < 0 || value > 100 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid weekly_remaining_min_percent", "message": "value must be between 0 and 100"})
			return
		}
		h.cfg.Routing.QuotaAware.WeeklyRemainingMinPercent = &value
	}
	h.persist(c)
}

// Proxy URL
func (h *Handler) GetProxyURL(c *gin.Context) { c.JSON(200, gin.H{"proxy-url": h.cfg.ProxyURL}) }
func (h *Handler) PutProxyURL(c *gin.Context) {
	h.updateStringField(c, func(v string) { h.cfg.ProxyURL = v })
}
func (h *Handler) DeleteProxyURL(c *gin.Context) {
	h.cfg.ProxyURL = ""
	h.persist(c)
}
