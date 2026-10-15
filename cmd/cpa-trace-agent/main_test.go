package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSettledLogFilesSkipsActiveMainLog(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	for _, name := range []string{"main.log", "main-2026-10-14T02-10-44.000.log", "v1-responses-abc.log", "notes.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	fresh := filepath.Join(dir, "v1-responses-fresh.log")
	if err := os.WriteFile(fresh, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := settledLogFiles(dir, time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"main-2026-10-14T02-10-44.000.log", "v1-responses-abc.log"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("settledLogFiles() = %v, want %v", files, want)
	}
}

func TestPollBillingExportsSuccessThatSupersedesFailedAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"event_id":"evt","completed_at":"2026-10-14T06:00:00Z","failed":true,"auth_index":"overloaded"},
			{"event_id":"evt","completed_at":"2026-10-14T06:00:05Z","failed":false,"auth_index":"rescuer"},
			{"event_id":"evt","completed_at":"2026-10-14T06:00:05Z","failed":false,"auth_index":"rescuer"}
		]`))
	}))
	defer server.Close()
	spool := t.TempDir()
	a := &agent{cfg: config{BaseURL: server.URL, BillingSpoolDir: spool, PollBatch: 10}, client: server.Client(), host: "test", billingSeen: make(map[string]struct{})}

	a.pollBillingOnce(context.Background())

	files, err := filepath.Glob(filepath.Join(spool, "*.ndjson"))
	if err != nil || len(files) != 1 {
		t.Fatalf("spool files = %v err=%v, want one", files, err)
	}
	f, err := os.Open(files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	for scanner := bufio.NewScanner(f); scanner.Scan(); {
		lines = append(lines, scanner.Text())
	}
	if len(lines) != 2 {
		t.Fatalf("spooled %d records, want failed attempt plus superseding success: %v", len(lines), lines)
	}
	if got := billingSeenKey([]byte(lines[1])); got != "evt" {
		t.Fatalf("second spooled record key = %q, want successful evt", got)
	}
	if _, ok := a.billingSeen["evt:failed"]; !ok {
		t.Fatal("failed attempt was not recorded in its own dedupe slot")
	}
}
