package api

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
)

//go:embed account_bridge.js
var accountBridge []byte

func accountBridgeURL() string {
	sum := sha256.Sum256(accountBridge)
	return "/account-bridge.js?v=" + hex.EncodeToString(sum[:8])
}

func (s *Server) serveAccountPortal(c *gin.Context) {
	if s.cfg == nil || s.cfg.Home.Enabled || s.cfg.RemoteManagement.DisableControlPanel {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	s.serveCachedUIAsset(c, filepath.Join(managementasset.StaticDir(s.configFilePath), "accounts.html"), "text/html; charset=utf-8", nil)
}

func (s *Server) serveAccountBridge(c *gin.Context) {
	s.serveCachedUIBytes(c, "account-bridge.js", "application/javascript; charset=utf-8", accountBridge)
}
