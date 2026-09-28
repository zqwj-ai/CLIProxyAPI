package cliproxy

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const startupReadinessTimeout = 60 * time.Second

type startupReadiness struct {
	ready   atomic.Bool
	scanned atomic.Bool
	authIDs []string // Published before scanned is set.
}

func (r *startupReadiness) setInitialAuths(auths []*coreauth.Auth) {
	seen := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		if auth == nil || auth.ID == "" || auth.Disabled {
			continue
		}
		if _, exists := seen[auth.ID]; !exists {
			r.authIDs = append(r.authIDs, auth.ID)
			seen[auth.ID] = struct{}{}
		}
	}
	r.scanned.Store(true)
}

func (r *startupReadiness) registrationsComplete(models *registry.ModelRegistry) bool {
	if !r.scanned.Load() || len(r.authIDs) == 0 {
		return false
	}
	for _, id := range r.authIDs {
		if len(models.GetModelsForClient(id)) == 0 {
			return false
		}
	}
	return true
}

func (r *startupReadiness) wait(ctx context.Context, models *registry.ModelRegistry, timeout time.Duration) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if r.registrationsComplete(models) {
			r.ready.Store(true)
			log.Info("startup ready: initial credentials and models registered")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-timer.C:
			r.ready.Store(true)
			log.Warn("startup readiness timed out; accepting inference requests after 60s")
			return
		}
	}
}

func (r *startupReadiness) middleware(c *gin.Context) {
	if r.ready.Load() || !startupInferenceRequest(c.Request) {
		c.Next()
		return
	}
	c.Header("Retry-After", "2")
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"type":  "error",
		"error": gin.H{"type": "overloaded_error", "message": "Proxy is starting; retry shortly"},
	})
}

func startupInferenceRequest(req *http.Request) bool {
	path := req.URL.Path
	if req.Method == http.MethodGet {
		return path == "/v1/responses" || path == "/backend-api/codex/responses" || path == "/v1/realtime" || path == "/v1/live"
	}
	if req.Method != http.MethodPost {
		return false
	}
	switch path {
	case "/v1/messages", "/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/responses/compact", "/v1/images/generations", "/v1/images/edits", "/v1/live", "/v1/realtime", "/v1beta/interactions", "/backend-api/codex/responses", "/backend-api/codex/responses/compact":
		return true
	}
	return strings.HasPrefix(path, "/v1beta/models/") || strings.HasPrefix(path, "/v1/videos")
}
