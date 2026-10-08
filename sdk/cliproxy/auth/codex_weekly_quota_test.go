package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSetWeeklyQuotaRemainingPercentUpdatesLiveAuth(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &Auth{
		ID:       "codex-live",
		Provider: "codex",
	})
	if err != nil || auth == nil {
		t.Fatalf("Register() auth=%#v err=%v", auth, err)
	}
	if !manager.SetWeeklyQuotaRemainingPercent(auth.ID, 72.4) {
		t.Fatal("SetWeeklyQuotaRemainingPercent() = false")
	}
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("auth missing after write")
	}
	if updated.Quota.Signals[WeeklyQuotaRemainingPercentSignal] != "72%" {
		t.Fatalf("signal = %#v", updated.Quota.Signals)
	}

	same := &Auth{
		ID:       "probed",
		Provider: "codex",
		Quota: QuotaState{Signals: map[string]string{
			WeeklyQuotaRemainingPercentSignal: "72%",
			"X-Codex-Primary-Used-Percent":    "0",
		}},
	}
	remaining, known := weeklyRemainingPercentForAuth(same)
	if !known || remaining != 72 {
		t.Fatalf("weekly remaining = %v known=%v, want 72", remaining, known)
	}

	selector := &QuotaAwareSelector{WeeklyRemainingMinPercent: 20}
	higher := &Auth{
		ID:       "higher",
		Provider: "codex",
		Quota:    QuotaState{Signals: map[string]string{WeeklyQuotaRemainingPercentSignal: "100%"}},
	}
	got, errPick := selector.Pick(context.Background(), "codex", "gpt-5.5", cliproxyexecutor.Options{}, []*Auth{same, higher})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got == nil || got.ID != higher.ID {
		t.Fatalf("Pick() = %#v, want higher weekly remaining", got)
	}
}

func TestSetWeeklyQuotaRemainingPercentRejectsNonCodex(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &Auth{ID: "claude-live", Provider: "claude"})
	if err != nil || auth == nil {
		t.Fatalf("Register() auth=%#v err=%v", auth, err)
	}
	if manager.SetWeeklyQuotaRemainingPercent(auth.ID, 50) {
		t.Fatal("non-codex write succeeded")
	}
}

func TestObserveResponseHeadersKeepsProbedWeeklyRemaining(t *testing.T) {
	t.Parallel()

	quota := QuotaState{
		ObservedAt: time.Unix(100, 0),
		Signals: map[string]string{
			WeeklyQuotaRemainingPercentSignal: "90%",
			"X-Codex-Primary-Used-Percent":    "10",
			"X-Codex-Primary-Window-Minutes":  "10080",
		},
	}
	if !quota.ObserveResponseHeadersForProvider("codex", http.Header{
		"X-Codex-Primary-Used-Percent":   []string{"40"},
		"X-Codex-Primary-Window-Minutes": []string{"300"},
	}, time.Unix(200, 0)) {
		t.Fatal("observation reported no change")
	}
	if quota.Signals[WeeklyQuotaRemainingPercentSignal] != "90%" {
		t.Fatalf("five-hour response erased weekly remaining: %#v", quota.Signals)
	}
	if quota.Signals["X-Codex-Primary-Used-Percent"] != "40" {
		t.Fatalf("primary used percent = %#v", quota.Signals)
	}
	if quota.Signals["X-Codex-Primary-Window-Minutes"] != "300" {
		t.Fatalf("primary window = %#v", quota.Signals)
	}
}

func TestObserveResponseHeadersUpdatesWeeklyRemainingFromSevenDayWindow(t *testing.T) {
	t.Parallel()

	quota := QuotaState{
		Signals: map[string]string{WeeklyQuotaRemainingPercentSignal: "90%"},
	}
	if !quota.ObserveResponseHeadersForProvider("codex", http.Header{
		"X-Codex-Primary-Used-Percent":     []string{"4"},
		"X-Codex-Primary-Window-Minutes":   []string{"300"},
		"X-Codex-Secondary-Used-Percent":   []string{"28"},
		"X-Codex-Secondary-Window-Minutes": []string{"10080"},
	}, time.Unix(200, 0)) {
		t.Fatal("observation reported no change")
	}
	if quota.Signals[WeeklyQuotaRemainingPercentSignal] != "72%" {
		t.Fatalf("weekly remaining = %#v, want 72%% from the seven-day secondary window", quota.Signals)
	}
}
