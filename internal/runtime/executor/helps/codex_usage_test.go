package helps

import "testing"

func TestCodexWeeklyRemainingPercent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		want      float64
		wantFound bool
	}{
		{
			name: "secondary weekly window",
			body: `{
				"rate_limit": {
					"primary_window": {"used_percent": 10, "limit_window_seconds": 18000, "reset_at": 1},
					"secondary_window": {"used_percent": 28, "limit_window_seconds": 604800, "reset_at": 2}
				}
			}`,
			want:      72,
			wantFound: true,
		},
		{
			name: "primary weekly window wins over five hour secondary",
			body: `{
				"rate_limit": {
					"primary_window": {"used_percent": 10, "limit_window_seconds": 604800, "reset_at": 1},
					"secondary_window": {"used_percent": 50, "limit_window_seconds": 18000, "reset_at": 2}
				}
			}`,
			want:      90,
			wantFound: true,
		},
		{
			name: "camel case",
			body: `{
				"rateLimit": {
					"primaryWindow": {"usedPercent": "4.6", "limitWindowSeconds": 18000, "resetAt": 1},
					"secondaryWindow": {"usedPercent": "27.6", "limitWindowSeconds": "604800", "resetAt": 2}
				}
			}`,
			want:      72.4,
			wantFound: true,
		},
		{
			name: "five hour only",
			body: `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":1}}}`,
		},
		{
			name: "monthly window is not weekly",
			body: `{"rate_limit":{"secondary_window":{"used_percent":10,"limit_window_seconds":2592000,"reset_at":1}}}`,
		},
		{
			name: "additional limit does not count as the account weekly window",
			body: `{
				"rate_limit": {"primary_window": {"used_percent": 10, "limit_window_seconds": 18000, "reset_at": 1}},
				"additional_rate_limits": [{"limit_name": "gpt-reserve", "rate_limit": {"primary_window": {"used_percent": 0, "limit_window_seconds": 604800}}}]
			}`,
		},
		{
			name:      "limit reached without used percent",
			body:      `{"rate_limit":{"limit_reached":true,"secondary_window":{"limit_window_seconds":604800,"reset_at":1700000000}}}`,
			want:      0,
			wantFound: true,
		},
		{
			name: "limit reached without a reset is not a reading",
			body: `{"rate_limit":{"limit_reached":true,"secondary_window":{"limit_window_seconds":604800}}}`,
		},
		{
			name:      "unused weekly window",
			body:      `{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1}}}`,
			want:      100,
			wantFound: true,
		},
		{name: "empty"},
		{name: "not json", body: "not-json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := CodexWeeklyRemainingPercent([]byte(tt.body))
			if ok != tt.wantFound {
				t.Fatalf("found = %v, want %v (remaining %v)", ok, tt.wantFound, got)
			}
			if ok && got != tt.want {
				t.Fatalf("remaining = %v, want %v", got, tt.want)
			}
		})
	}
}
