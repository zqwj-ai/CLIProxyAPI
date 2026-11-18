package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestStartupModelsRefreshCompletion(t *testing.T) {
	for _, outcome := range []string{"changed", "unchanged", "failed", "queued"} {
		t.Run(outcome, func(t *testing.T) {
			oldURLs, oldData := modelsURLs, getModels()
			refreshCallbackMu.Lock()
			oldCallback, oldPending := refreshCallback, pendingRefreshChanges
			refreshCallback, pendingRefreshChanges = nil, nil
			refreshCallbackMu.Unlock()
			t.Cleanup(func() {
				modelsURLs = oldURLs
				modelsCatalogStore.mu.Lock()
				modelsCatalogStore.data = oldData
				modelsCatalogStore.mu.Unlock()
				refreshCallbackMu.Lock()
				refreshCallback, pendingRefreshChanges = oldCallback, oldPending
				refreshCallbackMu.Unlock()
			})

			requested, releaseFetch := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblockFetch := func() { releaseOnce.Do(func() { close(releaseFetch) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(requested)
				<-releaseFetch
				switch outcome {
				case "failed":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "unchanged":
					_, _ = w.Write(embeddedModelsJSON)
				default:
					_, _ = w.Write([]byte(`{"claude":[{"id":"startup-remote-only-model"}]}`))
				}
			}))
			defer server.Close()
			defer unblockFetch()
			modelsURLs = []string{server.URL}
			if err := loadModelsFromBytes(embeddedModelsJSON, "test"); err != nil {
				t.Fatal(err)
			}

			embeddedData := getModels()
			callbackEntered, releaseCallback := make(chan struct{}), make(chan struct{})
			var callbackOnce sync.Once
			unblockCallback := func() { callbackOnce.Do(func() { close(releaseCallback) }) }
			defer unblockCallback()
			if outcome == "changed" {
				SetModelRefreshCallback(func([]string) {
					close(callbackEntered)
					<-releaseCallback
				})
			}
			ctx, cancel := context.WithCancel(context.Background())
			done, stopped := make(chan struct{}), make(chan struct{})
			defer func() {
				unblockFetch()
				unblockCallback()
				cancel()
				<-stopped
			}()
			go func() {
				defer close(stopped)
				runModelsUpdater(ctx, done)
			}()
			select {
			case <-requested:
			case <-time.After(2 * time.Second):
				t.Fatal("startup refresh did not request remote catalog")
			}
			select {
			case <-done:
				t.Fatal("startup refresh completed before remote fetch returned")
			default:
			}
			unblockFetch()
			if outcome == "changed" {
				select {
				case <-callbackEntered:
				case <-time.After(2 * time.Second):
					t.Fatal("refresh callback was not invoked")
				}
				select {
				case <-done:
					t.Fatal("startup refresh completed before model registrations finished")
				default:
				}
				unblockCallback()
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("startup refresh did not complete")
			}
			if outcome == "failed" && getModels() != embeddedData {
				t.Fatal("failed refresh did not retain embedded catalog")
			}
			if outcome == "queued" {
				applied := false
				SetModelRefreshCallback(func(providers []string) { applied = len(providers) > 0 })
				if !applied {
					t.Fatal("late callback registration did not synchronously apply queued catalog changes")
				}
			}
		})
	}
}

func TestDetectChangedProviders_KimiAliases(t *testing.T) {
	oldData := &staticModelsJSON{
		Kimi: []*ModelInfo{{ID: "kimi-k2"}},
	}
	newData := &staticModelsJSON{
		Kimi: []*ModelInfo{{ID: "kimi-k2"}, {ID: "kimi-k3"}},
	}

	changed := detectChangedProviders(oldData, newData)
	expected := map[string]bool{
		"kimi":     false,
		"kimi-ai":  false,
		"kimi.ai":  false,
		"kimi.com": false,
	}

	for _, p := range changed {
		if _, ok := expected[p]; ok {
			expected[p] = true
		}
	}

	for p, found := range expected {
		if !found {
			t.Errorf("expected changed provider %q to be reported, got %v", p, changed)
		}
	}
}
