package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const quotaTransitionModel = "quota-transition-model"

type quotaTransitionRefreshExecutor struct {
	schedulerProviderTestExecutor
}

func (e quotaTransitionRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	updated := auth.Clone()
	updated.Metadata["access_token"] = "rotated-access-token"
	return updated, nil
}

func newQuotaTransitionManager(t *testing.T, id string) *Manager {
	t.Helper()
	withQuotaCooldownEnabled(t)
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(quotaTransitionRefreshExecutor{schedulerProviderTestExecutor{provider: "codex"}})
	registerSchedulerModels(t, "codex", quotaTransitionModel, id)
	if _, err := manager.Register(context.Background(), &Auth{
		ID: id, Provider: "codex", Status: StatusActive,
		Metadata: map[string]any{"access_token": "old-access-token", "refresh_token": "refresh-token", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	retry := 2 * time.Hour
	manager.MarkResult(context.Background(), Result{
		AuthID: id, Provider: "codex", Model: quotaTransitionModel,
		Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exceeded"},
		CredentialScope: true, RetryAfter: &retry,
	})
	assertQuotaTransitionSelection(t, manager, id, false)
	return manager
}

func assertQuotaTransitionSelection(t *testing.T, manager *Manager, id string, wantAvailable bool) {
	t.Helper()
	current, ok := manager.GetByID(id)
	if !ok {
		t.Fatalf("auth %s missing", id)
	}
	blocked, _, _ := isAuthBlockedForModel(current, quotaTransitionModel, time.Now())
	if blocked == wantAvailable {
		t.Fatalf("auth %s blocked = %v, want %v; quota = %+v, model state = %+v", id, blocked, !wantAvailable, current.Quota, current.ModelStates[quotaTransitionModel])
	}
	selected, err := manager.scheduler.pickSingle(context.Background(), "codex", quotaTransitionModel, cliproxyexecutor.Options{}, nil)
	selectedID := ""
	if selected != nil {
		selectedID = selected.ID
	}
	if wantAvailable {
		if err != nil || selectedID != id {
			t.Fatalf("scheduler selected = %q, err = %v, want %s", selectedID, err, id)
		}
	} else if selectedID == id {
		t.Fatalf("scheduler selected cooling account %q", id)
	}
}

func TestCredentialQuotaNewImportDoesNotInheritDepletedAccount(t *testing.T) {
	manager := newQuotaTransitionManager(t, "quota-old")
	registerSchedulerModels(t, "codex", quotaTransitionModel, "quota-new")
	if _, err := manager.Register(context.Background(), &Auth{
		ID: "quota-new", Provider: "codex", Status: StatusActive,
		Metadata: map[string]any{"access_token": "new-account-token", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	assertQuotaTransitionSelection(t, manager, "quota-new", true)
	assertQuotaTransitionSelection(t, manager, "quota-old", false)
}

func TestCredentialQuotaDisableThenEnableClearsCooldown(t *testing.T) {
	manager := newQuotaTransitionManager(t, "quota-reenabled")
	current, _ := manager.GetByID("quota-reenabled")
	current.Disabled = true
	current.Status = StatusDisabled
	if _, err := manager.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	current, _ = manager.GetByID("quota-reenabled")
	current.Disabled = false
	current.Status = StatusActive
	if _, err := manager.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	assertQuotaTransitionSelection(t, manager, "quota-reenabled", true)
}

func TestCredentialQuotaEnabledUpdateWithoutCredentialChangeKeepsCooldown(t *testing.T) {
	manager := newQuotaTransitionManager(t, "quota-unchanged")
	current, _ := manager.GetByID("quota-unchanged")
	current.Label = "renamed"
	if _, err := manager.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	assertQuotaTransitionSelection(t, manager, "quota-unchanged", false)
}

func TestCredentialQuotaEnabledReplacementTokenClearsCooldown(t *testing.T) {
	manager := newQuotaTransitionManager(t, "quota-replaced")
	store := NewFileCooldownStateStore(t.TempDir())
	manager.SetCooldownStateStore(store)
	manager.PersistCooldownStates(context.Background())
	before, err := store.Load(context.Background())
	if err != nil || len(before) == 0 {
		t.Fatalf("cooldown records before replacement = %d, err = %v, want nonzero", len(before), err)
	}
	current, _ := manager.GetByID("quota-replaced")
	current.Metadata["access_token"] = "replacement-account-token"
	if _, err := manager.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	assertQuotaTransitionSelection(t, manager, "quota-replaced", true)
	after, err := store.Load(context.Background())
	if err != nil || len(after) != 0 {
		t.Fatalf("cooldown records after replacement = %d, err = %v, want zero", len(after), err)
	}
}

func TestCredentialQuotaForceRefreshKeepsDepletedAccountCooling(t *testing.T) {
	manager := newQuotaTransitionManager(t, "quota-refreshed")
	refreshed, err := manager.ForceRefreshAuth(context.Background(), "quota-refreshed")
	if err != nil {
		t.Fatal(err)
	}
	if got := refreshed.Metadata["access_token"]; got != "rotated-access-token" {
		t.Fatalf("refreshed access token = %v, want rotation", got)
	}
	assertQuotaTransitionSelection(t, manager, "quota-refreshed", false)
}
