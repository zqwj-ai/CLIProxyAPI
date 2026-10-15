package cliproxy

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestCodexAdditionalModelsAreAccountScoped(t *testing.T) {
	const model = "gpt-daybreak-blue-latest"
	service := &Service{cfg: &config.Config{}}
	registry := GlobalModelRegistry()
	for _, tc := range []struct {
		id       string
		metadata map[string]any
		excluded string
		want     bool
	}{
		{"entitled", map[string]any{"additional_models": []any{model, model, "", 42}}, "", true},
		{"ordinary", nil, "", false},
		{"excluded", map[string]any{"additional_models": []any{model}}, model, false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			id := "test-account-models-" + tc.id
			t.Cleanup(func() { registry.UnregisterClient(id) })
			auth := &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive,
				Metadata: tc.metadata, Attributes: map[string]string{"excluded_models": tc.excluded}}
			service.registerModelsForAuth(context.Background(), auth)
			models := registry.GetModelsForClient(id)
			count := 0
			for _, m := range models {
				if m.ID == model {
					count++
				}
			}
			if (count == 1) != tc.want || count > 1 {
				t.Fatalf("model count = %d, want present %v", count, tc.want)
			}
			if len(models) < 2 {
				t.Fatal("ordinary models disappeared")
			}
			if tc.want {
				delete(auth.Metadata, "additional_models")
				service.registerModelsForAuth(context.Background(), auth)
				for _, m := range registry.GetModelsForClient(id) {
					if m.ID == model {
						t.Fatal("removed account model remains registered")
					}
				}
			}
		})
	}
}
