package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"remnawave-traffic-limiter/internal/state"
)

func TestPairedWhiteListCreatesCompanionAndBlocksOnlyIt(t *testing.T) {
	const mainID, whiteID = 11, 22
	var updates []map[string]any
	whiteUsed := float64(0)
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/users":
			_, _ = w.Write([]byte(`{"response":{"id":22,"shortUuid":"white-short","username":"wl_main_11","status":"ACTIVE","trafficLimitBytes":1073741824,"trafficLimitStrategy":"NO_RESET","expireAt":"2026-12-01T00:00:00Z","activeInternalSquads":[{"uuid":"white"}],"userTraffic":{"usedTrafficBytes":0}}}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/api/users":
			var update map[string]any
			_ = json.NewDecoder(r.Body).Decode(&update)
			updates = append(updates, update)
			_, _ = w.Write([]byte(`{"response":{}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/22":
			_, _ = w.Write([]byte(`{"response":{"id":22,"shortUuid":"white-short","username":"wl_main_11","status":"ACTIVE","trafficLimitBytes":1073741824,"trafficLimitStrategy":"NO_RESET","expireAt":"2026-12-01T00:00:00Z","activeInternalSquads":[{"uuid":"white"}],"userTraffic":{"usedTrafficBytes":` + number(whiteUsed) + `}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/11":
			_, _ = w.Write([]byte(`{"response":{"id":11,"shortUuid":"main-short","username":"main","status":"ACTIVE","trafficLimitBytes":0,"trafficLimitStrategy":"NO_RESET","expireAt":"2026-12-01T00:00:00Z","activeInternalSquads":[{"uuid":"main"}],"userTraffic":{"usedTrafficBytes":0}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
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
	main := &User{ID: mainID, ShortUUID: "main-short", Username: "main", Status: "ACTIVE", TrafficLimitBytes: 1 << 30, TrafficLimitStrategy: "NO_RESET", ExpireAt: time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339), ActiveInternalSquads: []string{"main", "white"}}
	// A WhiteList-only tariff must remain one native panel user. It must not
	// create a technical companion or inject the Main squad.
	whiteOnlyQuota := int64(5 << 30)
	whiteOnlyStrategy, whiteOnlyExpiry, whiteOnlyStatus := "MONTH", "2026-12-15T00:00:00Z", "ACTIVE"
	whiteOnlySquads := []string{"white"}
	result, err := processor.ApplyPairedWhiteListIntent(main, PairingIntent{
		Enabled:              false,
		TrafficLimitBytes:    &whiteOnlyQuota,
		TrafficLimitStrategy: &whiteOnlyStrategy,
		ExpireAt:             &whiteOnlyExpiry,
		Status:               &whiteOnlyStatus,
		ActiveInternalSquads: &whiteOnlySquads,
	})
	if err != nil || result.Paired {
		t.Fatalf("WhiteList-only tariff must stay single: %#v, %v", result, err)
	}
	whiteOnlyWasRestored := false
	for _, update := range updates {
		squads, _ := update["activeInternalSquads"].([]any)
		if update["id"] == float64(mainID) && update["trafficLimitBytes"] == float64(whiteOnlyQuota) && len(squads) == 1 && squads[0] == "white" {
			whiteOnlyWasRestored = true
		}
	}
	if !whiteOnlyWasRestored {
		t.Fatalf("WhiteList-only target was not applied to Main: %#v", updates)
	}
	result, err = processor.ReconcilePairedWhiteList(&User{
		ID:                   33,
		ShortUUID:            "white-only",
		TrafficLimitBytes:    5 << 30,
		TrafficLimitStrategy: "MONTH",
		ActiveInternalSquads: []string{"white"},
	})
	if err != nil || result.Paired || result.WhiteUserID != 0 {
		t.Fatalf("background reconciliation must skip WhiteList-only user: %#v, %v", result, err)
	}
	result, err = processor.ReconcilePairedWhiteList(&User{
		ID:                   44,
		ShortUUID:            "expired-main-and-white",
		Status:               "ACTIVE",
		TrafficLimitBytes:    5 << 30,
		TrafficLimitStrategy: "MONTH",
		ExpireAt:             time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		ActiveInternalSquads: []string{"main", "white"},
	})
	if err != nil || result.Paired || result.WhiteUserID != 0 {
		t.Fatalf("background reconciliation must skip expired user: %#v, %v", result, err)
	}

	result, err = processor.ReconcilePairedWhiteList(main)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Paired || result.WhiteUserID != whiteID || result.State != state.StateActive {
		t.Fatalf("unexpected create result: %#v", result)
	}
	mainWasMadeUnlimited := false
	for _, update := range updates {
		if update["id"] == float64(mainID) && update["trafficLimitBytes"] == float64(0) {
			mainWasMadeUnlimited = true
		}
	}
	if !mainWasMadeUnlimited {
		t.Fatalf("main must be made unlimited: %#v", updates)
	}

	whiteUsed = 1 << 30
	main.TrafficLimitBytes = 0
	result, err = processor.ReconcilePairedWhiteList(main)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != state.StateBlocked {
		t.Fatalf("expected blocked, got %#v", result)
	}
	last := updates[len(updates)-1]
	squads, _ := last["activeInternalSquads"].([]any)
	if len(squads) != 1 || squads[0] != "notice" {
		t.Fatalf("WhiteList account must get notice: %#v", last)
	}

	// Remnawave sends the quota-exhausted event for the companion itself.
	// That event must reconcile immediately rather than wait for polling.
	result, err = processor.ReconcilePairedWhiteList(&User{ID: whiteID, ShortUUID: "white-short"})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != state.StateBlocked || result.MainUserID != mainID {
		t.Fatalf("technical webhook must reconcile its Main pair: %#v", result)
	}

	// After pairing, Main remains unlimited. A new tariff quota must therefore
	// come from the explicit Bedolaga transition intent, not from Main's zero.
	newQuota := int64(20 << 30)
	strategy, expiry, activeStatus := "MONTH", "2026-12-31T00:00:00Z", "ACTIVE"
	pairedSquads := []string{"main", "white"}
	result, err = processor.ApplyPairedWhiteListIntent(main, PairingIntent{
		Enabled:              true,
		TrafficLimitBytes:    &newQuota,
		TrafficLimitStrategy: &strategy,
		ExpireAt:             &expiry,
		Status:               &activeStatus,
		ActiveInternalSquads: &pairedSquads,
	})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := store.GetPairByMainUserID(mainID)
	if err != nil || pair.QuotaBytes != newQuota || !result.Paired {
		t.Fatalf("tariff intent must replace companion quota: %#v, %#v, %v", pair, result, err)
	}

	// A downgrade retains the companion but removes it from service. A later
	// LTE upgrade must reactivate that same account rather than create another.
	standardQuota := int64(50 << 30)
	standardSquads := []string{"main"}
	result, err = processor.ApplyPairedWhiteListIntent(main, PairingIntent{
		Enabled:              false,
		TrafficLimitBytes:    &standardQuota,
		TrafficLimitStrategy: &strategy,
		ExpireAt:             &expiry,
		Status:               &activeStatus,
		ActiveInternalSquads: &standardSquads,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Paired {
		t.Fatalf("disabled pair must not be exposed by the subscription gateway: %#v", result)
	}
	pair, err = store.GetPairByMainUserID(mainID)
	if err != nil || pair.Enabled || pair.WhiteUserID != whiteID {
		t.Fatalf("disabled pair must retain original technical user: %#v, %v", pair, err)
	}
	mainWasRestored := false
	for _, update := range updates {
		if update["id"] == float64(mainID) && update["trafficLimitBytes"] == float64(standardQuota) {
			mainWasRestored = true
		}
	}
	if !mainWasRestored {
		t.Fatalf("Main-only downgrade must restore its exact 50 GiB target: %#v", updates)
	}
	whiteWasDisabledAndCleared := false
	for _, update := range updates {
		if update["id"] == float64(whiteID) && update["status"] == "DISABLED" && update["trafficLimitBytes"] == float64(0) {
			whiteWasDisabledAndCleared = true
		}
	}
	if !whiteWasDisabledAndCleared {
		t.Fatalf("disabled companion must not retain a customer quota: %#v", updates)
	}
	result, err = processor.ApplyPairedWhiteListIntent(main, PairingIntent{
		Enabled:              true,
		TrafficLimitBytes:    &newQuota,
		TrafficLimitStrategy: &strategy,
		ExpireAt:             &expiry,
		Status:               &activeStatus,
		ActiveInternalSquads: &pairedSquads,
	})
	if err != nil {
		t.Fatal(err)
	}
	pair, err = store.GetPairByMainUserID(mainID)
	if err != nil || !pair.Enabled || pair.WhiteUserID != whiteID || !result.Paired {
		t.Fatalf("re-enabled pair must reuse original technical user: %#v, %#v, %v", pair, result, err)
	}
}

func number(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }
