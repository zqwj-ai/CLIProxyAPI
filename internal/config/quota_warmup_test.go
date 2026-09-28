package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQuotaWarmupConfigDefaultsAndDisable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct {
		name      string
		config    string
		codex     bool
		claude    bool
		mins      int
		backoff   int
		codexCap  int
		claudeCap int
	}{
		{"default", "port: 8317\n", true, true, 30, 360, 2, 6},
		{"disabled", "quota-warmup:\n  enabled: false\n", false, false, 30, 360, 2, 6},
		{"configured", "quota-warmup:\n  codex: false\n  interval-minutes: 15\n  retry-backoff-minutes: 90\n  codex-daily-cap: 3\n  claude-daily-cap: 8\n", false, true, 15, 90, 3, 8},
		{"explicit false", "quota-warmup:\n  claude: false\n", true, false, 30, 360, 2, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.QuotaWarmup.IsEnabled("codex") != tc.codex || cfg.QuotaWarmup.IsEnabled("claude") != tc.claude || cfg.QuotaWarmup.ScanIntervalMinutes() != tc.mins || cfg.QuotaWarmup.BackoffMinutes() != tc.backoff || cfg.QuotaWarmup.DailyCap("codex") != tc.codexCap || cfg.QuotaWarmup.DailyCap("claude") != tc.claudeCap || cfg.QuotaWarmup.IsEnabled("gemini") {
				t.Fatalf("config: %+v", cfg.QuotaWarmup)
			}
		})
	}
}
