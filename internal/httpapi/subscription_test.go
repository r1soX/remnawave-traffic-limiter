package httpapi

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"remnawave-traffic-limiter/internal/engine"
)

func TestMergeURILists(t *testing.T) {
	main := []byte(base64.StdEncoding.EncodeToString([]byte("vless://main\n")))
	white := []byte("vless://white")
	merged, err := mergeURILists(main, white)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(merged))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(decoded); got != "vless://main\nvless://white" {
		t.Fatalf("unexpected merged payload: %q", got)
	}
}

func TestUserSubscriptionInfoUsesServedUserLimit(t *testing.T) {
	user := &engine.User{
		TrafficLimitBytes: 50 << 30,
		UsedTrafficBytes:  2 << 30,
		ExpireAt:          "2026-12-01T00:00:00Z",
	}
	info := userSubscriptionInfo(user)
	if !strings.Contains(info, "download=2147483648") || !strings.Contains(info, "total=53687091200") {
		t.Fatalf("expected native Main limit in subscription header, got %q", info)
	}
}

func TestMergeURIListsRejectsOpaqueContent(t *testing.T) {
	if _, err := mergeURILists([]byte("opaque"), []byte("vless://white")); err == nil {
		t.Fatal("expected opaque subscription to be rejected")
	}
}

func TestSubscriptionUserInfo(t *testing.T) {
	info := subscriptionUserInfo(42, 100, "2026-12-01T00:00:00Z")
	for _, want := range []string{"download=42", "total=100", "expire="} {
		if !strings.Contains(info, want) {
			t.Fatalf("userinfo %q does not contain %q", info, want)
		}
	}
}

func TestWriteSubscriptionPreservesMetadataHeaders(t *testing.T) {
	response := &subscriptionResponse{
		body: []byte("vless://merged"),
		header: http.Header{
			"Profile-Title":         {"Original profile"},
			"X-Custom-Subscription": {"kept"},
			"Content-Length":        {"1"},
			"ETag":                  {"old-body"},
			"Set-Cookie":            {"session=upstream-secret"},
			"Subscription-Userinfo": {"download=1; total=1"},
		},
		status: http.StatusOK,
	}
	recorder := httptest.NewRecorder()
	writeSubscription(recorder, response, "download=42; total=100")

	if got := recorder.Header().Get("X-Custom-Subscription"); got != "kept" {
		t.Fatalf("custom header = %q, want kept", got)
	}
	if got := recorder.Header().Get("Profile-Title"); got != "Original profile" {
		t.Fatalf("profile title = %q", got)
	}
	if got := recorder.Header().Get("Subscription-Userinfo"); got != "download=42; total=100" {
		t.Fatalf("userinfo = %q", got)
	}
	for _, forbidden := range []string{"Content-Length", "ETag", "Set-Cookie"} {
		if got := recorder.Header().Get(forbidden); got != "" {
			t.Fatalf("unexpected %s header: %q", forbidden, got)
		}
	}
}

func TestAcceptsHTML(t *testing.T) {
	if !acceptsHTML(http.Header{"Accept": {"text/html,application/xhtml+xml"}}) {
		t.Fatal("browser Accept header was not recognized")
	}
	if acceptsHTML(http.Header{"Accept": {"application/json"}}) {
		t.Fatal("non-browser Accept header was recognized as HTML")
	}
}
