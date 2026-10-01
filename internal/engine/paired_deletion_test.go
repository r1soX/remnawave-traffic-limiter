package engine

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"remnawave-traffic-limiter/internal/state"
)

func TestPairedUserDeletionCascadesOnlyFromMain(t *testing.T) {
	deleteStatus := http.StatusNoContent
	deleteCalls := 0
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/users/22" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		deleteCalls++
		w.WriteHeader(deleteStatus)
	}))
	defer panel.Close()

	store, err := state.NewSQLite(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	processor, err := NewProcessor(panel.URL, "token", "main", "white")
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.ConfigurePairedWhiteList(store, "notice"); err != nil {
		t.Fatal(err)
	}
	pair := state.PairedUser{
		MainUserID: 11, MainShortUUID: "main-short",
		WhiteUserID: 22, WhiteShortUUID: "white-short",
		TrafficStrategy: "MONTH", State: state.StateActive, Enabled: true,
	}
	if err := store.UpsertPair(pair); err != nil {
		t.Fatal(err)
	}

	handled, err := processor.HandlePairedUserDeleted(11, "main-short")
	if err != nil || !handled {
		t.Fatalf("main deletion was not handled: handled=%v err=%v", handled, err)
	}
	if deleteCalls != 1 {
		t.Fatalf("expected one companion deletion, got %d", deleteCalls)
	}
	if _, err := store.GetPairByMainUserID(11); err != sql.ErrNoRows {
		t.Fatalf("pair state must be removed after cascade, got %v", err)
	}

	// A separately deleted technical account must never cascade back to Main.
	if err := store.UpsertPair(pair); err != nil {
		t.Fatal(err)
	}
	handled, err = processor.HandlePairedUserDeleted(0, "white-short")
	if err != nil || !handled {
		t.Fatalf("technical deletion was not handled: handled=%v err=%v", handled, err)
	}
	if deleteCalls != 1 {
		t.Fatalf("technical deletion must not delete Main; calls=%d", deleteCalls)
	}
	if _, err := store.GetPairByMainUserID(11); err != sql.ErrNoRows {
		t.Fatalf("stale pair state must be removed, got %v", err)
	}

	// Keep the mapping when Remnawave rejects the companion deletion so the
	// next reconciliation tick can retry it.
	if err := store.UpsertPair(pair); err != nil {
		t.Fatal(err)
	}
	deleteStatus = http.StatusInternalServerError
	handled, err = processor.HandlePairedUserDeleted(11, "")
	if err == nil || !handled {
		t.Fatalf("failed cascade must be reported: handled=%v err=%v", handled, err)
	}
	if _, err := store.GetPairByMainUserID(11); err != nil {
		t.Fatalf("pair state must be retained for retry: %v", err)
	}
}
