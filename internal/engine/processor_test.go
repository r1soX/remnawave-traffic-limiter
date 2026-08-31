package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProcessUserLimitedRemovesWhitelistOnly(t *testing.T) {
	var patchPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/users/resolve":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"response":{"id":7,"username":"alice","shortUuid":"abc123"}}`))
		case "/api/users":
			if r.Method != http.MethodPatch {
				t.Fatalf("expected PATCH method, got %s", r.Method)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if err := json.Unmarshal(body, &patchPayload); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	p, err := NewProcessor(server.URL, "token", "main-uuid", "whitelist-uuid", "", "")
	if err != nil {
		t.Fatal(err)
	}

	user := &User{
		ID:                   7,
		ShortUUID:            "abc123",
		Username:             "alice",
		Status:               "ACTIVE",
		TrafficLimitBytes:    1000,
		TrafficLimitStrategy: "NO_RESET",
		ActiveInternalSquads: []string{"main-uuid", "whitelist-uuid", "other-uuid"},
	}
	if err := p.HandleUserLimited(user); err != nil {
		t.Fatal(err)
	}
	if patchPayload == nil {
		t.Fatal("expected patch payload to be sent")
	}
	if active, ok := patchPayload["activeInternalSquads"].([]any); ok {
		for _, item := range active {
			if item == "whitelist-uuid" {
				t.Fatal("whitelist uuid should be removed from patch payload")
			}
		}
		foundMain := false
		for _, item := range active {
			if item == "main-uuid" {
				foundMain = true
			}
		}
		if !foundMain {
			t.Fatal("main uuid should remain in patch payload")
		}
		return
	}
	t.Fatal("activeInternalSquads not found in payload")
}
