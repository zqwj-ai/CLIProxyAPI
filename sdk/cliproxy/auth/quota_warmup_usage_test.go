package auth

import "testing"

func TestParseCodexWarmupUsage(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		unstarted bool
		exhausted bool
		wantError bool
	}{
		{"idle weekly primary", `{"rate_limit":{"limit_reached":false,"primary_window":{"used_percent":0,"reset_after_seconds":604800,"limit_window_seconds":604800,"reset_at":1791163650},"secondary_window":null}}`, true, false, false},
		{"running at zero percent", `{"rate_limit":{"primary_window":{"used_percent":0,"reset_after_seconds":604764,"limit_window_seconds":604800}}}`, false, false, false},
		{"used window", `{"rate_limit":{"primary_window":{"used_percent":25,"reset_after_seconds":604800,"limit_window_seconds":604800}}}`, false, false, false},
		{"idle secondary", `{"rate_limit":{"primary_window":{"used_percent":12,"reset_after_seconds":10,"limit_window_seconds":18000},"secondary_window":{"used_percent":0,"reset_after_seconds":604800,"limit_window_seconds":604800}}}`, true, false, false},
		{"limit reached", `{"rate_limit":{"limit_reached":true,"primary_window":{"used_percent":0,"reset_after_seconds":604800,"limit_window_seconds":604800}}}`, true, true, false},
		{"no window", `{"rate_limit":{"primary_window":null,"secondary_window":null}}`, false, false, true},
		{"malformed", `not json`, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage, err := parseCodexWarmupUsage([]byte(tc.body))
			if (err != nil) != tc.wantError || usage.Unstarted != tc.unstarted || usage.Exhausted != tc.exhausted {
				t.Fatalf("usage=%+v err=%v", usage, err)
			}
		})
	}
}

func TestParseClaudeWarmupUsage(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		unstarted bool
		exhausted bool
		wantError bool
	}{
		{"idle five-hour", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":71,"resets_at":"2026-10-01T00:00:00Z"}}`, true, false, false},
		{"idle weekly", `{"five_hour":{"utilization":25,"resets_at":"2026-09-29T00:00:00Z"},"seven_day":{"utilization":0,"resets_at":""}}`, true, false, false},
		{"both running", `{"five_hour":{"utilization":25,"resets_at":"2026-09-29T00:00:00Z"},"seven_day":{"utilization":71,"resets_at":"2026-10-01T00:00:00Z"}}`, false, false, false},
		{"missing resets_at", `{"five_hour":{"utilization":0}}`, true, false, false},
		{"exhausted", `{"five_hour":{"utilization":100,"resets_at":"2026-09-29T00:00:00Z"}}`, false, true, false},
		{"weekly exhausted with idle five-hour", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":100,"resets_at":"2026-10-01T00:00:00Z"}}`, true, true, false},
		{"absent windows", `{}`, false, false, true},
		{"malformed", `{oops`, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage, err := parseClaudeWarmupUsage([]byte(tc.body))
			if (err != nil) != tc.wantError || usage.Unstarted != tc.unstarted || usage.Exhausted != tc.exhausted {
				t.Fatalf("usage=%+v err=%v", usage, err)
			}
		})
	}
}
