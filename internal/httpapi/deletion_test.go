package httpapi

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"remnawave-traffic-limiter/internal/config"
	"remnawave-traffic-limiter/internal/engine"
	"remnawave-traffic-limiter/internal/state"
	"remnawave-traffic-limiter/internal/webhook"
)

func TestDeletedMainWebhookDeletesWhiteListCompanion(t *testing.T) {
	deleteCalls := 0
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/users/22" {
			t.Fatalf("unexpected panel request: %s %s", r.Method, r.URL.Path)
		}
		deleteCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer panel.Close()

	store, err := state.NewSQLite(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertPair(state.PairedUser{
		MainUserID: 11, MainShortUUID: "main-short",
		WhiteUserID: 22, WhiteShortUUID: "white-short",
		TrafficStrategy: "MONTH", State: state.StateActive, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	processor, err := engine.NewProcessor(panel.URL, "panel-token", "main", "white")
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.ConfigurePairedWhiteList(store, "notice"); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:   &config.Config{EnablePairedWhiteList: true, PairedWhiteListManageAll: true},
		store: store,
		proc:  processor,
	}

	server.processWebhookEvent(&webhook.Event{
		Event: "user.deleted",
		Data:  webhook.EventData{ID: 11, ShortUUID: "main-short"},
	})
	if deleteCalls != 1 {
		t.Fatalf("expected one companion deletion, got %d", deleteCalls)
	}
	if _, err := store.GetPairByMainUserID(11); err != sql.ErrNoRows {
		t.Fatalf("pair state must be removed, got %v", err)
	}
}

func TestReconcileCleansPairWhenMainWebhookWasMissed(t *testing.T) {
	deleteCalls := 0
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/users":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"response":{"users":[{"id":22,"shortUuid":"white-short","username":"wl_main_11","status":"ACTIVE","trafficLimitBytes":1,"trafficLimitStrategy":"MONTH","expireAt":"2026-12-01T00:00:00Z","tag":"WL_LIMITER","activeInternalSquads":[{"uuid":"white"}]}],"total":1}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/users/22":
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected panel request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer panel.Close()

	store, err := state.NewSQLite(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertPair(state.PairedUser{
		MainUserID: 11, MainShortUUID: "main-short",
		WhiteUserID: 22, WhiteShortUUID: "white-short",
		TrafficStrategy: "MONTH", State: state.StateActive, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	processor, err := engine.NewProcessor(panel.URL, "panel-token", "main", "white")
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.ConfigurePairedWhiteList(store, "notice"); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:   &config.Config{EnablePairedWhiteList: true, PairedWhiteListManageAll: true},
		store: store,
		proc:  processor,
	}

	if err := server.ReconcileWhiteListUsers(); err != nil {
		t.Fatal(err)
	}
	if deleteCalls != 1 {
		t.Fatalf("expected one orphan cleanup, got %d", deleteCalls)
	}
	if _, err := store.GetPairByMainUserID(11); err != sql.ErrNoRows {
		t.Fatalf("orphaned pair state must be removed, got %v", err)
	}
}
