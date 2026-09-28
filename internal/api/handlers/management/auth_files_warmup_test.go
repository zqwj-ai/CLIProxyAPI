package management

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type managementWarmupExecutor struct{ calls int }

func (*managementWarmupExecutor) Identifier() string { return "codex" }
func (*managementWarmupExecutor) Execute(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
	return executor.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (*managementWarmupExecutor) ExecuteStream(context.Context, *coreauth.Auth, executor.Request, executor.Options) (*executor.StreamResult, error) {
	return nil, nil
}
func (*managementWarmupExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}
func (*managementWarmupExecutor) CountTokens(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
	return executor.Response{}, nil
}
func (e *managementWarmupExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	e.calls++
	body := `{"rate_limit":{"primary_window":{"used_percent":0,"reset_after_seconds":604800,"limit_window_seconds":604800}}}`
	if e.calls > 1 {
		body = `{"rate_limit":{"primary_window":{"used_percent":0,"reset_after_seconds":604790,"limit_window_seconds":604800}}}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestListAuthFilesIncludesQuotaWarmupStatus(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	path := filepath.Join(authDir, "sample.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex"}`), 0600); err != nil {
		t.Fatal(err)
	}
	m := coreauth.NewManager(nil, nil, nil)
	e := &managementWarmupExecutor{}
	m.RegisterExecutor(e)
	_, err := m.Register(context.Background(), &coreauth.Auth{
		ID: "warmup-auth", FileName: "sample.json", Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{"path": path, coreauth.AttributeAuthKind: coreauth.AuthKindOAuth},
		Metadata:   map[string]any{"access_token": "test-only-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartQuotaWarmup(ctx)
	defer m.StopQuotaWarmup()
	deadline := time.After(2 * time.Second)
	for {
		status, ok := m.WarmupStatus("warmup-auth")
		if ok && status.CheckResult == "warmed" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("warmup loop did not report confirmed status")
		case <-time.After(10 * time.Millisecond):
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, m)
	payload := requestAuthFilesPage(t, h, "/v0/management/auth-files?page=1&page_size=1")
	if len(payload.Files) != 1 {
		t.Fatalf("file count=%d", len(payload.Files))
	}
	status, ok := payload.Files[0]["quota_warmup"].(map[string]any)
	if !ok || status["check_result"] != "warmed" || status["last_attempt_result"] != "warmed" {
		t.Fatalf("warmup status=%v", status)
	}
}
