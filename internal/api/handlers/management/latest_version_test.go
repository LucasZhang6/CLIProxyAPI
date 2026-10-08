package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestGetLatestVersionCachesAndServesStaleWhileRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		n := hits.Load()
		tag := "v1.0.0"
		if n > 1 {
			tag = "v2.0.0"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})
	}))
	t.Cleanup(upstream.Close)

	now := time.Unix(1_700_000_000, 0)
	h := &Handler{}
	h.latestVersion.endpoint = upstream.URL
	h.latestVersion.nowFunc = func() time.Time { return now }

	r := gin.New()
	r.GET("/latest-version", h.GetLatestVersion)

	first := httptest.NewRecorder()
	r.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/latest-version", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	if !jsonHasLatest(t, first.Body.Bytes(), "v1.0.0") {
		t.Fatalf("first body=%s", first.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d, want 1", hits.Load())
	}

	second := httptest.NewRecorder()
	r.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/latest-version", nil))
	if second.Code != http.StatusOK || !jsonHasLatest(t, second.Body.Bytes(), "v1.0.0") {
		t.Fatalf("cached body=%s status=%d", second.Body.String(), second.Code)
	}
	if hits.Load() != 1 {
		t.Fatalf("fresh cache should not hit GitHub again, hits=%d", hits.Load())
	}

	now = now.Add(latestVersionCacheTTL + time.Second)
	stale := httptest.NewRecorder()
	r.ServeHTTP(stale, httptest.NewRequest(http.MethodGet, "/latest-version", nil))
	if stale.Code != http.StatusOK || !jsonHasLatest(t, stale.Body.Bytes(), "v1.0.0") {
		t.Fatalf("stale body=%s status=%d", stale.Body.String(), stale.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() < 2 {
		t.Fatalf("expected background refresh, hits=%d", hits.Load())
	}
}

func jsonHasLatest(t *testing.T, body []byte, want string) bool {
	t.Helper()
	var payload map[string]string
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	return payload["latest-version"] == want
}
