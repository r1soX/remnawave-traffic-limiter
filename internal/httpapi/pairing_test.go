package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"remnawave-traffic-limiter/internal/config"
	"remnawave-traffic-limiter/internal/engine"
	"remnawave-traffic-limiter/internal/state"
)

func TestPairingGETDoesNotRequirePOSTBody(t *testing.T) {
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/users/by-short-uuid/main-short" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":{"id":11,"shortUuid":"main-short","username":"main","status":"ACTIVE","trafficLimitBytes":0,"trafficLimitStrategy":"MONTH","expireAt":"2026-12-01T00:00:00Z","activeInternalSquads":[{"uuid":"main"}],"userTraffic":{"usedTrafficBytes":0}}}`))
	}))
	defer panel.Close()

	store, err := state.NewSQLite(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	processor, err := engine.NewProcessor(panel.URL, "panel-token", "main", "white")
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.ConfigurePairedWhiteList(store, "notice"); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg: &config.Config{
			APIPath:                     "/api",
			EnablePairedWhiteList:       true,
			PairedWhiteListManageAll:    true,
			PairedWhiteListControlToken: "control-token",
		},
		store: store,
		proc:  processor,
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/pairing/main-short", nil)
	request.Header.Set("X-Paired-Whitelist-Token", "control-token")
	server.handlePairing(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; response=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
}

func TestPairingPOSTStillRequiresFullTarget(t *testing.T) {
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":{"id":11,"shortUuid":"main-short","status":"ACTIVE"}}`))
	}))
	defer panel.Close()

	store, err := state.NewSQLite(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	processor, err := engine.NewProcessor(panel.URL, "panel-token", "main", "white")
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.ConfigurePairedWhiteList(store, "notice"); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg: &config.Config{
			APIPath:                     "/api",
			EnablePairedWhiteList:       true,
			PairedWhiteListManageAll:    true,
			PairedWhiteListControlToken: "control-token",
		},
		store: store,
		proc:  processor,
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/pairing/main-short", nil)
	request.Header.Set("X-Paired-Whitelist-Token", "control-token")
	server.handlePairing(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}
