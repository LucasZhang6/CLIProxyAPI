package api

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
)

const managementAssetMinExtract = 2048

type managementBundleStore struct {
	mu    sync.RWMutex
	files map[string]*uiAssetCacheEntry
}

func (s *managementBundleStore) replace(assets ...*uiAssetCacheEntry) {
	files := make(map[string]*uiAssetCacheEntry, len(assets))
	for _, asset := range assets {
		if asset == nil || asset.file.path == "" {
			continue
		}
		files[asset.file.path] = asset
	}
	s.mu.Lock()
	s.files = files
	s.mu.Unlock()
}

func (s *managementBundleStore) lookup(name string) *uiAssetCacheEntry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.files[name]
}

func (s *Server) transformManagementHTML(payload []byte) []byte {
	html := injectManagementBillingNav(payload)
	rewritten, assets := extractManagementBundles(html)
	s.managementBundles.replace(assets...)
	return rewritten
}

func (s *Server) serveManagementAsset(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if !validManagementAssetName(name) {
		c.AbortWithStatus(404)
		return
	}
	entry := s.managementBundles.lookup(name)
	if entry == nil {
		s.warmManagementBundles()
		entry = s.managementBundles.lookup(name)
	}
	if entry == nil {
		c.AbortWithStatus(404)
		return
	}
	writeUIAssetResponse(c, entry, uiAssetImmutableCacheControl)
}

func (s *Server) warmManagementBundles() {
	if s == nil {
		return
	}
	filePath := managementasset.FilePath(s.configFilePath)
	if strings.TrimSpace(filePath) == "" {
		return
	}
	payload, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	_ = s.transformManagementHTML(payload)
}

func validManagementAssetName(name string) bool {
	if len(name) != 19 && len(name) != 20 {
		return false
	}
	dot := strings.LastIndexByte(name, '.')
	if dot != 16 {
		return false
	}
	ext := name[dot+1:]
	if ext != "js" && ext != "css" {
		return false
	}
	for _, r := range name[:dot] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func extractManagementBundles(html []byte) ([]byte, []*uiAssetCacheEntry) {
	source := string(html)
	var out strings.Builder
	var assets []*uiAssetCacheEntry
	i := 0
	for i < len(source) {
		next, tag := nextExtractableTag(source, i)
		if next < 0 {
			out.WriteString(source[i:])
			break
		}
		out.WriteString(source[i:next])
		openEnd, attrs, ok := readOpenTag(source, next, tag)
		if !ok {
			out.WriteString(source[next:])
			break
		}
		closeStart, closeEnd, ok := findCloseTag(source, openEnd, tag)
		if !ok {
			out.WriteString(source[next:])
			break
		}
		body := source[openEnd:closeStart]
		if htmlTagHasAttr(attrs, "src") || htmlTagHasAttr(attrs, "href") || len(body) < managementAssetMinExtract {
			out.WriteString(source[next:closeEnd])
			i = closeEnd
			continue
		}
		asset, err := newHashedUIAsset(tag, body)
		if err != nil {
			out.WriteString(source[next:closeEnd])
			i = closeEnd
			continue
		}
		assets = append(assets, asset)
		if tag == "script" {
			out.WriteString("<script")
			out.WriteString(keepScriptAttrs(attrs))
			out.WriteString(` src="/management.assets/`)
			out.WriteString(asset.file.path)
			out.WriteString(`"></script>`)
		} else {
			out.WriteString(`<link rel="stylesheet" href="/management.assets/`)
			out.WriteString(asset.file.path)
			out.WriteString(`" crossorigin>`)
		}
		i = closeEnd
	}
	return []byte(out.String()), assets
}

func nextExtractableTag(source string, start int) (int, string) {
	script := indexHTMLTag(source, start, "script")
	style := indexHTMLTag(source, start, "style")
	switch {
	case script < 0 && style < 0:
		return -1, ""
	case script < 0:
		return style, "style"
	case style < 0:
		return script, "script"
	case script < style:
		return script, "script"
	default:
		return style, "style"
	}
}

func indexHTMLTag(source string, start int, tag string) int {
	needle := "<" + tag
	from := start
	for {
		idx := strings.Index(strings.ToLower(source[from:]), needle)
		if idx < 0 {
			return -1
		}
		pos := from + idx
		after := pos + len(needle)
		if after >= len(source) {
			return -1
		}
		next := rune(source[after])
		if next == '>' || next == '/' || unicode.IsSpace(next) {
			return pos
		}
		from = after
	}
}

func readOpenTag(source string, start int, tag string) (int, string, bool) {
	if start < 0 || start >= len(source) || source[start] != '<' {
		return 0, "", false
	}
	end := strings.IndexByte(source[start:], '>')
	if end < 0 {
		return 0, "", false
	}
	end += start + 1
	open := source[start:end]
	lower := strings.ToLower(open)
	if !strings.HasPrefix(lower, "<"+tag) {
		return 0, "", false
	}
	attrs := strings.TrimSpace(open[1+len(tag) : len(open)-1])
	return end, attrs, true
}

func findCloseTag(source string, from int, tag string) (int, int, bool) {
	needle := "</" + tag + ">"
	idx := strings.Index(strings.ToLower(source[from:]), needle)
	if idx < 0 {
		return 0, 0, false
	}
	start := from + idx
	return start, start + len(needle), true
}

func htmlTagHasAttr(attrs, name string) bool {
	lower := strings.ToLower(attrs)
	needle := strings.ToLower(name)
	for _, part := range strings.Fields(lower) {
		key, _, _ := strings.Cut(part, "=")
		if key == needle {
			return true
		}
	}
	return false
}

func keepScriptAttrs(attrs string) string {
	if strings.TrimSpace(attrs) == "" {
		return ""
	}
	var kept []string
	for _, part := range fieldsHTMLAttrs(attrs) {
		key, _, _ := strings.Cut(part, "=")
		switch strings.ToLower(key) {
		case "type", "crossorigin", "nomodule", "async", "defer":
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return " " + strings.Join(kept, " ")
}

func fieldsHTMLAttrs(attrs string) []string {
	var parts []string
	var b strings.Builder
	inQuote := byte(0)
	for i := 0; i < len(attrs); i++ {
		ch := attrs[i]
		if inQuote != 0 {
			b.WriteByte(ch)
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			inQuote = ch
			b.WriteByte(ch)
			continue
		}
		if unicode.IsSpace(rune(ch)) {
			if b.Len() > 0 {
				parts = append(parts, b.String())
				b.Reset()
			}
			continue
		}
		b.WriteByte(ch)
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts
}

func newHashedUIAsset(kind, body string) (*uiAssetCacheEntry, error) {
	ext := "js"
	contentType := "application/javascript; charset=utf-8"
	if kind == "style" {
		ext = "css"
		contentType = "text/css; charset=utf-8"
	}
	sum := sha256.Sum256([]byte(body))
	name := hex.EncodeToString(sum[:])[:16] + "." + ext
	return newUIAssetCacheEntry(name, uiAssetFileIdentity{path: name, size: int64(len(body))}, nil, contentType, []byte(body))
}
