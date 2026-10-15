package cliproxy

import "strings"

// appendCodexAccountModels registers explicitly configured upstream model IDs only
// for this OAuth account. It supplements plan defaults; it does not grant upstream
// access or rewrite model names. Exclusions and aliases are applied afterwards.
func appendCodexAccountModels(models []*ModelInfo, metadata map[string]any) []*ModelInfo {
	ids, ok := metadata["additional_models"].([]any)
	if !ok {
		return models
	}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if model != nil {
			seen[model.ID] = true
		}
	}
	for _, value := range ids {
		id, ok := value.(string)
		id = strings.TrimSpace(id)
		if !ok || id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, &ModelInfo{
			ID: id, Object: "model", OwnedBy: "openai", Type: "codex",
			DisplayName: id, UserDefined: true,
		})
	}
	return models
}
