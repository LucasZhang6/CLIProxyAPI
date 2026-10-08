package api

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractManagementBundlesSplitsLargeInlineAssets(t *testing.T) {
	js := strings.Repeat("console.log(1);", 200)
	css := strings.Repeat("body{color:red}", 200)
	html := `<html><head><script type="module" crossorigin>` + js + `</script><style>` + css + `</style></head><body><div id="root"></div></body></html>`
	out, assets := extractManagementBundles([]byte(html))
	if len(assets) != 2 {
		t.Fatalf("assets=%d, want 2", len(assets))
	}
	rewritten := string(out)
	if strings.Contains(rewritten, js) || strings.Contains(rewritten, css) {
		t.Fatal("extracted bodies should leave the HTML shell")
	}
	if !strings.Contains(rewritten, `type="module"`) || !strings.Contains(rewritten, `src="/management.assets/`) {
		t.Fatalf("missing module script src: %s", rewritten)
	}
	if !strings.Contains(rewritten, `rel="stylesheet"`) {
		t.Fatalf("missing stylesheet link: %s", rewritten)
	}
	if !validManagementAssetName(assets[0].file.path) || !validManagementAssetName(assets[1].file.path) {
		t.Fatalf("invalid asset names: %q %q", assets[0].file.path, assets[1].file.path)
	}
}

func TestExtractManagementBundlesKeepsSmallInlineAndExternal(t *testing.T) {
	html := `<html><head><script src="/account-bridge.js"></script><script>ok()</script></head><body></body></html>`
	out, assets := extractManagementBundles([]byte(html))
	if len(assets) != 0 {
		t.Fatalf("assets=%d, want 0", len(assets))
	}
	if string(out) != html {
		t.Fatalf("small/external tags should be unchanged\n%s", out)
	}
}

func TestTransformManagementHTMLKeepsBridgeBeforeBundle(t *testing.T) {
	js := strings.Repeat("legacyLogin();", 300)
	original := `<html><head><script type="module">` + js + `</script></head><body></body></html>`
	var server Server
	output := string(server.transformManagementHTML([]byte(original)))
	bridge := strings.Index(output, `id="cpa-account-bridge"`)
	marker := `/management.assets/`
	bundle := strings.Index(output, marker)
	if bridge < 0 || bundle < 0 || bridge > bundle {
		t.Fatalf("bridge must precede extracted bundle: %s", output)
	}
	if !strings.Contains(output, "cpa-billing-nav-link") {
		t.Fatal("billing nav missing after transform")
	}
	nameStart := bundle + len(marker)
	nameEnd := strings.IndexAny(output[nameStart:], `"' >`)
	if nameEnd < 0 {
		t.Fatalf("cannot parse asset name from %s", output)
	}
	name := output[nameStart : nameStart+nameEnd]
	if server.managementBundles.lookup(name) == nil {
		t.Fatalf("extracted bundle not stored: %q", name)
	}
}

func TestManagementHTMLServesSplitHashedAssets(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	js := strings.Repeat("window.CPA=1;", 400)
	writeUIAsset(t, filepath.Join(staticDir, "management.html"), `<html><head><script type="module">`+js+`</script></head><body><main>split app</main></body></html>`)
	server := newTestServer(t)

	page := performUIAssetRequest(t, server, http.MethodGet, "/management.html", map[string]string{"Accept-Encoding": "br"})
	if page.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", page.Code, page.Body.String())
	}
	if page.Header().Get("Cache-Control") != uiAssetCacheControl {
		t.Fatalf("Cache-Control=%q", page.Header().Get("Cache-Control"))
	}
	if page.Header().Get("Content-Encoding") != "br" {
		t.Fatalf("Content-Encoding=%q, want br", page.Header().Get("Content-Encoding"))
	}
	html := string(unbrotliTestBody(t, page.Body.Bytes()))
	if strings.Contains(html, js) {
		t.Fatal("inline bundle should be extracted from HTML")
	}
	marker := `/management.assets/`
	idx := strings.Index(html, marker)
	if idx < 0 {
		t.Fatalf("missing asset url: %s", html)
	}
	nameStart := idx + len(marker)
	nameEnd := strings.IndexAny(html[nameStart:], `"' >`)
	name := html[nameStart : nameStart+nameEnd]
	asset := performUIAssetRequest(t, server, http.MethodGet, "/management.assets/"+name, map[string]string{"Accept-Encoding": "br"})
	if asset.Code != http.StatusOK {
		t.Fatalf("asset status=%d", asset.Code)
	}
	if asset.Header().Get("Cache-Control") != uiAssetImmutableCacheControl {
		t.Fatalf("asset Cache-Control=%q", asset.Header().Get("Cache-Control"))
	}
	if string(unbrotliTestBody(t, asset.Body.Bytes())) != js {
		t.Fatal("hashed asset body mismatch")
	}
	head := performUIAssetRequest(t, server, http.MethodHead, "/management.html", map[string]string{"If-None-Match": page.Header().Get("ETag"), "Accept-Encoding": "br"})
	if head.Code != http.StatusNotModified {
		t.Fatalf("HEAD If-None-Match status=%d, want 304", head.Code)
	}
}
