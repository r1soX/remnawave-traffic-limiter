package remnawave

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveUserByShortUuid(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/users/by-short-uuid/abc123" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":{"id":42,"username":"alice","shortUuid":"abc123"}}`))
	}))
	defer server.Close()

	c, err := NewClient(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}

	user, err := c.ResolveUser("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != 42 {
		t.Fatalf("expected id 42, got %d", user.ID)
	}
	if user.ShortUUID != "abc123" {
		t.Fatalf("expected shortUuid abc123, got %s", user.ShortUUID)
	}
	if user.Username != "alice" {
		t.Fatalf("expected username alice, got %s", user.Username)
	}
}

func TestResetUserTrafficUsesEmptyPostBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/users/42/actions/reset-traffic" {
			t.Fatalf("unexpected reset request: %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) != 0 {
			t.Fatalf("reset request must have no JSON body, got %q", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c, err := NewClient(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ResetUserTraffic(42); err != nil {
		t.Fatal(err)
	}
}
