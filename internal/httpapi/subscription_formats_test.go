package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseSubscriptionPath(t *testing.T) {
	tests := []struct {
		path       string
		shortUUID  string
		clientType string
		ok         bool
	}{
		{"/sub/user_01", "user_01", "", true},
		{"/sub/user_01/mihomo", "user_01", "mihomo", true},
		{"/sub/user_01/singbox", "user_01", "singbox", true},
		{"/sub/user_01/unknown", "", "", false},
		{"/sub/user_01/mihomo/extra", "", "", false},
		{"/wrong/user_01", "", "", false},
	}
	for _, test := range tests {
		shortUUID, clientType, ok := parseSubscriptionPath(test.path)
		if shortUUID != test.shortUUID || clientType != test.clientType || ok != test.ok {
			t.Fatalf("parseSubscriptionPath(%q) = (%q, %q, %t), want (%q, %q, %t)", test.path, shortUUID, clientType, ok, test.shortUUID, test.clientType, test.ok)
		}
	}
}

func TestFetchSubscriptionKeepsClientFormatSelection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/user_01/mihomo" {
			t.Fatalf("upstream path = %q, want /user_01/mihomo", got)
		}
		if got := r.Header.Get("User-Agent"); got != "ClashMetaForAndroid" {
			t.Fatalf("User-Agent = %q", got)
		}
		if got := r.Header.Get("X-Remnawave-Limiter-Gateway"); got != "1" {
			t.Fatalf("gateway marker = %q", got)
		}
		w.Header().Set("Content-Type", "application/x-yaml")
		_, _ = io.WriteString(w, "proxies: []\nproxy-groups: []\n")
	}))
	defer upstream.Close()

	response, err := fetchSubscription(upstream.URL, "user_01", "mihomo", http.Header{
		"User-Agent": {"ClashMetaForAndroid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := response.header.Get("Content-Type"); got != "application/x-yaml" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestMergeYAMLSubscriptions(t *testing.T) {
	main := []byte(`
proxies:
  - name: Main NL
    type: vless
proxy-groups:
  - name: Auto
    type: select
    proxies: [Main NL, DIRECT]
rules: [MATCH,Auto]
`)
	white := []byte(`
proxies:
  - name: WhiteList NL
    type: vless
proxy-groups:
  - name: Auto
    type: select
    proxies: [WhiteList NL, DIRECT]
rules: [MATCH,Auto]
`)
	merged, err := mergeYAMLSubscriptions(main, white)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(merged, &document); err != nil {
		t.Fatal(err)
	}
	proxies := document["proxies"].([]any)
	if len(proxies) != 2 {
		t.Fatalf("proxies count = %d, want 2", len(proxies))
	}
	group := document["proxy-groups"].([]any)[0].(map[string]any)
	members := group["proxies"].([]any)
	if len(members) != 3 || members[0] != "Main NL" || members[1] != "DIRECT" || members[2] != "WhiteList NL" {
		t.Fatalf("unexpected group members: %#v", members)
	}
}

func TestMergeJSONSubscriptions(t *testing.T) {
	main := []byte(`{
  "outbounds": [
    {"tag":"Auto","type":"selector","outbounds":["Main NL"]},
    {"tag":"Main NL","type":"vless","server":"main.example"}
  ],
  "route": {"auto_detect_interface":true}
}`)
	white := []byte(`{
  "outbounds": [
    {"tag":"Auto","type":"selector","outbounds":["WhiteList NL"]},
    {"tag":"WhiteList NL","type":"vless","server":"white.example"}
  ],
  "route": {"auto_detect_interface":true}
}`)
	merged, err := mergeJSONSubscriptions(main, white)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Outbounds []struct {
			Tag       string   `json:"tag"`
			Outbounds []string `json:"outbounds"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(merged, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Outbounds) != 3 {
		t.Fatalf("outbounds count = %d, want 3", len(document.Outbounds))
	}
	if got := document.Outbounds[0].Outbounds; strings.Join(got, ",") != "Main NL,WhiteList NL" {
		t.Fatalf("selector outbounds = %#v", got)
	}
}

func TestMergeSubscriptionsUsesResponseContentType(t *testing.T) {
	main := &subscriptionResponse{
		body:   []byte("proxies: []\nproxy-groups: []\n"),
		header: http.Header{"Content-Type": {"application/x-yaml; charset=utf-8"}},
	}
	white := &subscriptionResponse{
		body:   []byte("proxies: []\nproxy-groups: []\n"),
		header: http.Header{"Content-Type": {"application/x-yaml"}},
	}
	merged, err := mergeSubscriptions(main, white)
	if err != nil || !strings.Contains(string(merged), "proxies") {
		t.Fatalf("mergeSubscriptions YAML = %q, %v", merged, err)
	}
}

func TestMergeJSONSubscriptionsRejectsConflictingNodeTag(t *testing.T) {
	main := []byte(`{"outbounds":[{"tag":"same","type":"vless","server":"one"}]}`)
	white := []byte(`{"outbounds":[{"tag":"same","type":"vless","server":"two"}]}`)
	if _, err := mergeJSONSubscriptions(main, white); err == nil {
		t.Fatal("expected conflicting tag to be rejected")
	}
}
