package cliproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/claude"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestStartupReadinessMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gate := &startupReadiness{}
	router := gin.New()
	router.Use(gate.middleware)
	router.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	router.POST("/v1/messages", func(c *gin.Context) {
		if c.Query("model") == "missing-model" {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": "msg-test"})
	})

	request := func(path string, method string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		return w
	}
	if w := request("/healthz", http.MethodGet); w.Code != http.StatusOK {
		t.Fatalf("healthz before ready = %d, want 200", w.Code)
	}
	before := request("/v1/messages", http.MethodPost)
	if before.Code != http.StatusServiceUnavailable || before.Header().Get("Retry-After") != "2" {
		t.Fatalf("before ready = %d, retry-after=%q", before.Code, before.Header().Get("Retry-After"))
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(before.Body.Bytes(), &body); err != nil || body.Type != "error" || body.Error.Type != "overloaded_error" {
		t.Fatalf("before ready error: %q (%v)", before.Body.String(), err)
	}

	gate.ready.Store(true)
	if w := request("/v1/messages", http.MethodPost); w.Code != http.StatusOK {
		t.Fatalf("registered model after ready = %d, want 200", w.Code)
	}
	if w := request("/v1/messages?model=missing-model", http.MethodPost); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown model after ready = %d, want 400", w.Code)
	}
}

func TestStartupReadinessWaitsForInitialRegistrations(t *testing.T) {
	models := registry.GetGlobalRegistry()
	const first, second = "startup-ready-test-first", "startup-ready-test-second"
	t.Cleanup(func() { models.UnregisterClient(first); models.UnregisterClient(second) })
	gate := &startupReadiness{}
	if gate.registrationsComplete(models) {
		t.Fatal("ready before initial credential scan")
	}
	gate.setInitialAuths([]*coreauth.Auth{{ID: first}, {ID: second}, {ID: "disabled", Disabled: true}})
	if gate.registrationsComplete(models) {
		t.Fatal("ready with empty model registry")
	}
	models.RegisterClient(first, "codex", []*registry.ModelInfo{{ID: "startup-first-model"}})
	if gate.registrationsComplete(models) {
		t.Fatal("ready with second credential unregistered")
	}
	models.RegisterClient(second, "claude", []*registry.ModelInfo{{ID: "startup-second-model"}})
	if !gate.registrationsComplete(models) {
		t.Fatal("not ready after every enabled credential registered models")
	}
}

func TestStartupReadinessWaitsForRemoteOnlyModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	models := registry.GetGlobalRegistry()
	const authID = "startup-remote-model-auth"
	const remoteModel = "startup-remote-only-codex-model"
	t.Cleanup(func() { models.UnregisterClient(authID) })

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&syncTestExecutor{})
	auth := &coreauth.Auth{
		ID: authID, Provider: "codex", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": `{"id":"msg-startup-ready","type":"message","content":[]}`},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	models.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "startup-embedded-model"}})
	refreshDone := make(chan struct{})
	gate := &startupReadiness{modelsRefreshDone: refreshDone}
	gate.setInitialAuths([]*coreauth.Auth{auth})
	if gate.registrationsComplete(models) {
		t.Fatal("ready with embedded registrations while startup refresh is pending")
	}

	router := gin.New()
	router.Use(gate.middleware)
	handler := claude.NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
	router.POST("/v1/messages", handler.ClaudeMessages)
	request := func(model string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		body := `{"model":"` + model + `","max_tokens":1,"messages":[{"role":"user","content":"test"}]}`
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		return w
	}
	if w := request(remoteModel); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("remote-only model before refresh = %d, retry-after=%q, body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}

	// Model registrations are rebuilt synchronously before refresh completion is published.
	models.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "startup-embedded-model"}, {ID: remoteModel}})
	manager.RefreshSchedulerEntry(authID)
	if gate.registrationsComplete(models) {
		t.Fatal("ready before startup refresh callback completed")
	}
	close(refreshDone)
	// A zero timeout also proves the success path runs before timeout fallback.
	gate.wait(context.Background(), models, 0)
	if !gate.registrationsComplete(models) || !gate.ready.Load() {
		t.Fatal("not ready after startup remote model registration completed")
	}
	if w := request(remoteModel); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "msg-startup-ready") {
		t.Fatalf("remote-only model after refresh = %d, body=%s", w.Code, w.Body.String())
	}
	if w := request("startup-truly-unknown-model"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknown provider") {
		t.Fatalf("unknown model after refresh = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestStartupReadinessTimeoutFailsOpen(t *testing.T) {
	gate := &startupReadiness{modelsRefreshDone: make(chan struct{})}
	gate.wait(context.Background(), registry.GetGlobalRegistry(), time.Nanosecond)
	if !gate.ready.Load() {
		t.Fatal("timeout left inference unavailable")
	}
}
