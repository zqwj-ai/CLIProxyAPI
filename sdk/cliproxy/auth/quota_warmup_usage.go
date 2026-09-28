package auth

import (
	"errors"

	"github.com/tidwall/gjson"
)

// quotaWarmupUsage holds only non-secret values needed for the warmup decision and log.
type quotaWarmupUsage struct {
	Unstarted   bool
	Window      string
	Utilization float64
	ResetAt     int64
	Exhausted   bool
}

func parseCodexWarmupUsage(body []byte) (quotaWarmupUsage, error) {
	if !gjson.ValidBytes(body) {
		return quotaWarmupUsage{}, errors.New("invalid codex usage response")
	}
	limits := gjson.GetBytes(body, "rate_limit")
	if !limits.IsObject() {
		return quotaWarmupUsage{}, errors.New("missing codex rate limit")
	}
	usage := quotaWarmupUsage{Exhausted: limits.Get("limit_reached").Bool()}
	found := false
	for _, name := range []string{"primary_window", "secondary_window"} {
		window := limits.Get(name)
		if !window.IsObject() {
			continue
		}
		used := window.Get("used_percent")
		resetAfter := window.Get("reset_after_seconds")
		limit := window.Get("limit_window_seconds")
		if used.Type != gjson.Number || resetAfter.Type != gjson.Number || limit.Type != gjson.Number || limit.Float() <= 0 {
			continue
		}
		found = true
		if usage.Window == "" {
			usage.Window = name
			usage.Utilization = used.Float()
			usage.ResetAt = window.Get("reset_at").Int()
		}
		if used.Float() == 0 && resetAfter.Float() == limit.Float() {
			usage.Unstarted = true
			usage.Window = name
			usage.Utilization = 0
			usage.ResetAt = window.Get("reset_at").Int()
			break
		}
	}
	if !found {
		return quotaWarmupUsage{}, errors.New("missing codex quota window")
	}
	return usage, nil
}

func parseClaudeWarmupUsage(body []byte) (quotaWarmupUsage, error) {
	if !gjson.ValidBytes(body) {
		return quotaWarmupUsage{}, errors.New("invalid claude usage response")
	}
	root := gjson.ParseBytes(body)
	usage := quotaWarmupUsage{}
	found := false
	for _, name := range []string{"five_hour", "seven_day"} {
		window := root.Get(name)
		if !window.IsObject() {
			continue
		}
		found = true
		utilization := window.Get("utilization")
		if usage.Window == "" {
			usage.Window = name
			if utilization.Type == gjson.Number {
				usage.Utilization = utilization.Float()
			}
		}
		if utilization.Type == gjson.Number && utilization.Float() >= 100 {
			usage.Exhausted = true
		}
		reset := window.Get("resets_at")
		if !usage.Unstarted && (!reset.Exists() || reset.Type == gjson.Null || reset.String() == "") {
			usage.Unstarted = true
			usage.Window = name
			if utilization.Type == gjson.Number {
				usage.Utilization = utilization.Float()
			}
		}
	}
	if !found {
		return quotaWarmupUsage{}, errors.New("missing claude quota window")
	}
	return usage, nil
}
