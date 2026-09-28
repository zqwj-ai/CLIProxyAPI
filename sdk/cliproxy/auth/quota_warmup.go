package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const (
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
)

// QuotaWarmupStatus is an in-memory, credential-scoped observation. It contains
// neither credential material nor upstream response bodies.
type QuotaWarmupStatus struct {
	CheckedAt         time.Time `json:"checked_at"`
	CheckResult       string    `json:"check_result"`
	LastAttemptAt     time.Time `json:"last_attempt_at,omitempty"`
	LastAttemptResult string    `json:"last_attempt_result,omitempty"`
	Window            string    `json:"window,omitempty"`
	UsedPercent       float64   `json:"used_percent"`
	ResetAt           int64     `json:"reset_at,omitempty"`
}

// WarmupStatus returns the last probe and warmup attempt for one credential.
func (m *Manager) WarmupStatus(id string) (QuotaWarmupStatus, bool) {
	if m == nil {
		return QuotaWarmupStatus{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	status, ok := m.warmupStatus[id]
	return status, ok
}

// StartQuotaWarmup scans immediately and then at the configured interval. It is
// deliberately independent of request selection and cooldown bookkeeping.
func (m *Manager) StartQuotaWarmup(parent context.Context) {
	if m == nil || m.HomeEnabled() {
		return
	}
	m.mu.Lock()
	previous := m.warmupCancel
	ctx, cancel := context.WithCancel(parent)
	m.warmupCancel = cancel
	m.mu.Unlock()
	if previous != nil {
		previous()
	}
	intervalMinutes := 30
	if cfg := m.runtimeConfigSnapshot(); cfg != nil {
		intervalMinutes = cfg.QuotaWarmup.ScanIntervalMinutes()
	}
	log.Infof("quota warmup loop started (interval=%dm)", intervalMinutes)
	go func() {
		for {
			m.scanQuotaWarmup(ctx)
			cfg := m.runtimeConfigSnapshot()
			interval := 30 * time.Minute
			if cfg != nil {
				interval = time.Duration(cfg.QuotaWarmup.ScanIntervalMinutes()) * time.Minute
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (m *Manager) StopQuotaWarmup() {
	if m == nil {
		return
	}
	m.mu.Lock()
	cancel := m.warmupCancel
	m.warmupCancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) scanQuotaWarmup(ctx context.Context) {
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil {
		cfg = &config.Config{}
	}
	if !cfg.QuotaWarmup.IsEnabled("codex") && !cfg.QuotaWarmup.IsEnabled("claude") {
		return
	}
	for _, auth := range m.List() {
		if ctx.Err() != nil {
			return
		}
		if auth == nil || !cfg.QuotaWarmup.IsEnabled(auth.Provider) || !quotaWarmupEligible(auth, time.Now()) {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		m.warmupAuth(probeCtx, auth)
		cancel()
	}
}

func quotaWarmupEligible(auth *Auth, now time.Time) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || auth.Unavailable || !auth.HasValidAccessToken(now) || auth.AuthKind() != AuthKindOAuth {
		return false
	}
	if auth.Quota.Exceeded || strings.EqualFold(auth.Quota.Signals["X-Codex-Limit-Reached"], "true") ||
		(!auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now)) ||
		(!auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now)) {
		return false
	}
	for _, state := range auth.ModelStates {
		if isModelStateActiveCooldown(state, now) {
			return false
		}
	}
	return true
}

func (m *Manager) warmupAuth(ctx context.Context, auth *Auth) {
	usage, err := m.probeWarmupUsage(ctx, auth)
	if err != nil {
		m.recordWarmupStatus(auth, "probe_failed", quotaWarmupUsage{}, false)
		return
	}
	if usage.Exhausted {
		m.recordWarmupStatus(auth, "limit_reached", usage, false)
		return
	}
	if !usage.Unstarted {
		m.recordWarmupStatus(auth, "already_running", usage, false)
		return
	}
	// A request may have changed this credential since the scan/probe began.
	current, ok := m.GetByID(auth.ID)
	cfg := m.runtimeConfigSnapshot()
	if !ok || !quotaWarmupEligible(current, time.Now()) || (cfg != nil && !cfg.QuotaWarmup.IsEnabled(current.Provider)) {
		return
	}
	if err = m.executeWarmup(ctx, current); err != nil {
		m.recordWarmupStatus(current, "warmup_failed", usage, true)
		return
	}
	verified, err := m.probeWarmupUsage(ctx, current)
	if err != nil || verified.Unstarted {
		m.recordWarmupStatus(current, "confirmation_failed", usage, true)
		return
	}
	m.recordWarmupStatus(current, "warmed", verified, true)
}

func (m *Manager) probeWarmupUsage(ctx context.Context, auth *Auth) (quotaWarmupUsage, error) {
	url := codexUsageURL
	if auth.Provider == "claude" {
		url = claudeUsageURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return quotaWarmupUsage{}, err
	}
	if auth.Provider == "claude" {
		req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
		req.Header.Set("Anthropic-Version", "2023-06-01")
	} else if accountID, ok := auth.Metadata["account_id"].(string); ok && accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	resp, err := m.HttpRequest(ctx, auth, req)
	if err != nil {
		return quotaWarmupUsage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return quotaWarmupUsage{}, fmt.Errorf("usage status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return quotaWarmupUsage{}, err
	}
	if auth.Provider == "claude" {
		return parseClaudeWarmupUsage(body)
	}
	return parseCodexWarmupUsage(body)
}

func (m *Manager) executeWarmup(ctx context.Context, auth *Auth) error {
	exec := m.executorFor(executorKeyFromAuth(auth))
	if exec == nil {
		return fmt.Errorf("warmup executor unavailable")
	}
	model, payload, format := "gpt-6-luna", `{"model":"gpt-6-luna","input":"Hi","max_output_tokens":16}`, translator.FromString("openai-response")
	if auth.Provider == "claude" {
		model, payload, format = "claude-haiku-4-5-20251001", `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"probe"}]}`, translator.FromString("claude")
	}
	if rt := m.roundTripperFor(auth); rt != nil {
		ctx = context.WithValue(ctx, roundTripperContextKey{}, rt)
		ctx = context.WithValue(ctx, "cliproxy.roundtripper", rt)
	}
	_, err := exec.Execute(ctx, auth.Clone(), executor.Request{Model: model, Payload: []byte(payload), Format: format}, executor.Options{SourceFormat: format})
	return err
}

func (m *Manager) recordWarmupStatus(auth *Auth, result string, usage quotaWarmupUsage, attempted bool) {
	at := time.Now().UTC()
	m.mu.Lock()
	if m.warmupStatus == nil {
		m.warmupStatus = make(map[string]QuotaWarmupStatus)
	}
	status := m.warmupStatus[auth.ID]
	status.CheckedAt = at
	status.CheckResult = result
	status.Window = usage.Window
	status.UsedPercent = usage.Utilization
	status.ResetAt = usage.ResetAt
	if attempted {
		status.LastAttemptAt = at
		status.LastAttemptResult = result
	}
	m.warmupStatus[auth.ID] = status
	m.mu.Unlock()

	marker := auth.FileName
	if marker == "" {
		marker = auth.ID
	}
	digest := sha256.Sum256([]byte(filepath.Base(marker)))
	// The production LogFormatter prints only an allowlist of structured fields.
	// Put safe result/usage values in the message so operators can verify the loop.
	log.Infof("quota warmup result=%s window=%s used_pct=%.2f reset_at=%d provider=%s credential=%x",
		result, usage.Window, usage.Utilization, usage.ResetAt, auth.Provider, digest[:4])
}
