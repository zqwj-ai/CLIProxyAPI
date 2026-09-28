package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	idleCodexUsage    = `{"rate_limit":{"limit_reached":false,"primary_window":{"used_percent":0,"reset_after_seconds":604800,"limit_window_seconds":604800,"reset_at":1791163650},"secondary_window":{"used_percent":0,"reset_after_seconds":18000,"limit_window_seconds":18000}}}`
	runningCodexUsage = `{"rate_limit":{"limit_reached":false,"primary_window":{"used_percent":0,"reset_after_seconds":604780,"limit_window_seconds":604800,"reset_at":1791163650}}}`
)

type warmupTestExecutor struct {
	provider string
	usage    []string
	probes   int
	attempts int
	fail     bool
	ids      []string
	models   []string
	payloads []string
}

func (e *warmupTestExecutor) Identifier() string { return e.provider }
func (e *warmupTestExecutor) Execute(_ context.Context, auth *Auth, req executor.Request, _ executor.Options) (executor.Response, error) {
	e.attempts++
	e.ids = append(e.ids, auth.ID)
	e.models = append(e.models, req.Model)
	e.payloads = append(e.payloads, string(req.Payload))
	if e.fail {
		return executor.Response{}, errors.New("upstream failure -- must not enter auth state")
	}
	return executor.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (e *warmupTestExecutor) ExecuteStream(context.Context, *Auth, executor.Request, executor.Options) (*executor.StreamResult, error) {
	return nil, nil
}
func (e *warmupTestExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) { return a, nil }
func (e *warmupTestExecutor) CountTokens(context.Context, *Auth, executor.Request, executor.Options) (executor.Response, error) {
	return executor.Response{}, nil
}
func (e *warmupTestExecutor) HttpRequest(_ context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	if auth.ID == "" || req.Method != http.MethodGet {
		return nil, errors.New("missing credential or incorrect method")
	}
	if e.provider == "codex" && (req.URL.String() != codexUsageURL || req.Header.Get("Chatgpt-Account-Id") != "test-account") {
		return nil, errors.New("codex usage headers or endpoint incorrect")
	}
	if e.provider == "claude" && (req.URL.String() != claudeUsageURL || req.Header.Get("Anthropic-Beta") != "oauth-2025-04-20") {
		return nil, errors.New("claude usage headers or endpoint incorrect")
	}
	index := e.probes
	e.probes++
	if len(e.usage) == 0 {
		return nil, errors.New("probe failed")
	}
	if index >= len(e.usage) {
		index = len(e.usage) - 1
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(e.usage[index]))}, nil
}

func registerWarmupAuth(t *testing.T, m *Manager, id, provider string) *Auth {
	t.Helper()
	a, err := m.Register(context.Background(), &Auth{
		ID: id, Provider: provider, Status: StatusActive,
		Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth},
		Metadata:   map[string]any{"access_token": "test-only-token", "account_id": "test-account"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestQuotaWarmupScansAndConfirmsOnce(t *testing.T) {
	m := NewManager(nil, nil, nil)
	e := &warmupTestExecutor{provider: "codex", usage: []string{idleCodexUsage, runningCodexUsage}}
	m.RegisterExecutor(e)
	a := registerWarmupAuth(t, m, "idle-codex", "codex")
	m.scanQuotaWarmup(context.Background())
	if e.probes != 2 || e.attempts != 1 || len(e.ids) != 1 || e.ids[0] != a.ID || e.models[0] != "gpt-6-luna" || !strings.Contains(e.payloads[0], `"max_output_tokens":16`) {
		t.Fatalf("probes=%d attempts=%d ids=%v models=%v", e.probes, e.attempts, e.ids, e.models)
	}
	status, ok := m.WarmupStatus(a.ID)
	if !ok || status.CheckResult != "warmed" || status.LastAttemptResult != "warmed" || status.ResetAt != 1791163650 {
		t.Fatalf("status=%+v ok=%v", status, ok)
	}
	m.scanQuotaWarmup(context.Background())
	status, _ = m.WarmupStatus(a.ID)
	if e.attempts != 1 || status.CheckResult != "already_running" || status.LastAttemptResult != "warmed" {
		t.Fatalf("second scan attempts=%d status=%+v", e.attempts, status)
	}
}

func TestQuotaWarmupSkipDisabledCoolingExhaustedAndSwitches(t *testing.T) {
	falseValue := false
	for _, tc := range []struct {
		name       string
		authChange func(*Auth)
		config     config.QuotaWarmupConfig
		usage      string
		wantProbes int
	}{
		{"global off", nil, config.QuotaWarmupConfig{Enabled: &falseValue}, idleCodexUsage, 0},
		{"codex off", nil, config.QuotaWarmupConfig{Codex: &falseValue}, idleCodexUsage, 0},
		{"disabled", func(a *Auth) { a.Disabled = true }, config.QuotaWarmupConfig{}, idleCodexUsage, 0},
		{"cooling", func(a *Auth) { a.NextRetryAfter = time.Now().Add(time.Hour) }, config.QuotaWarmupConfig{}, idleCodexUsage, 0},
		{"model cooling", func(a *Auth) {
			a.ModelStates = map[string]*ModelState{"gpt-6-luna": {NextRetryAfter: time.Now().Add(time.Hour)}}
		}, config.QuotaWarmupConfig{}, idleCodexUsage, 0},
		{"quota exhausted", func(a *Auth) { a.Quota.Exceeded = true }, config.QuotaWarmupConfig{}, idleCodexUsage, 0},
		{"observed limit reached", func(a *Auth) { a.Quota.Signals = map[string]string{"X-Codex-Limit-Reached": "true"} }, config.QuotaWarmupConfig{}, idleCodexUsage, 0},
		{"upstream limit reached", nil, config.QuotaWarmupConfig{}, strings.Replace(idleCodexUsage, `"limit_reached":false`, `"limit_reached":true`, 1), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.SetConfig(&config.Config{QuotaWarmup: tc.config})
			e := &warmupTestExecutor{provider: "codex", usage: []string{tc.usage}}
			m.RegisterExecutor(e)
			a := &Auth{ID: "candidate", Provider: "codex", Status: StatusActive, Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth}, Metadata: map[string]any{"access_token": "test-only-token", "account_id": "test-account"}}
			if tc.authChange != nil {
				tc.authChange(a)
			}
			if _, err := m.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			m.scanQuotaWarmup(context.Background())
			if e.probes != tc.wantProbes || e.attempts != 0 {
				t.Fatalf("probes=%d attempts=%d", e.probes, e.attempts)
			}
		})
	}
}

func TestQuotaWarmupFailureDoesNotChangeRoutingState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		usage  []string
		fail   bool
		result string
	}{
		{"probe error", nil, false, "probe_failed"},
		{"warmup error", []string{idleCodexUsage}, true, "warmup_failed"},
		{"confirmation error", []string{idleCodexUsage}, false, "confirmation_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			e := &warmupTestExecutor{provider: "codex", usage: tc.usage, fail: tc.fail}
			m.RegisterExecutor(e)
			a := registerWarmupAuth(t, m, "test-auth", "codex")
			m.scanQuotaWarmup(context.Background())
			status, _ := m.WarmupStatus(a.ID)
			current, _ := m.GetByID(a.ID)
			if status.CheckResult != tc.result || current.Status != StatusActive || current.Unavailable || current.NextRetryAfter != (time.Time{}) || current.Quota.Exceeded || current.Success != 0 || current.Failed != 0 {
				t.Fatalf("result=%s status=%s unavailable=%v retry=%v exceeded=%v success=%d failed=%d", status.CheckResult, current.Status, current.Unavailable, current.NextRetryAfter, current.Quota.Exceeded, current.Success, current.Failed)
			}
		})
	}
}

func TestClaudeQuotaWarmupUsesHaiku(t *testing.T) {
	m := NewManager(nil, nil, nil)
	e := &warmupTestExecutor{provider: "claude", usage: []string{`{"five_hour":{"utilization":0,"resets_at":null}}`, `{"five_hour":{"utilization":0,"resets_at":"2026-09-29T00:00:00Z"}}`}}
	m.RegisterExecutor(e)
	registerWarmupAuth(t, m, "claude-auth", "claude")
	m.scanQuotaWarmup(context.Background())
	if e.attempts != 1 || e.probes != 2 || e.models[0] != "claude-haiku-4-5" || !strings.Contains(e.payloads[0], `"max_tokens":1`) {
		t.Fatalf("attempts=%d probes=%d models=%v", e.attempts, e.probes, e.models)
	}
}
