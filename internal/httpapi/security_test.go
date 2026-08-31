package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"remnawave-traffic-limiter/internal/config"
)

func TestDiagnosticsEndpointsRequireToken(t *testing.T) {
	server := &Server{cfg: &config.Config{DiagnosticsToken: "test-diagnostics-token"}}
	for name, testCase := range map[string]struct {
		handler http.HandlerFunc
		method  string
		path    string
	}{
		"state":     {handler: server.handleState, method: http.MethodGet, path: "/api/state/example"},
		"reconcile": {handler: server.handleReconcile, method: http.MethodPost, path: "/api/reconcile/example"},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(testCase.method, testCase.path, nil)
			testCase.handler(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestAuthorizeDiagnosticsAcceptsOnlyMatchingToken(t *testing.T) {
	server := &Server{cfg: &config.Config{DiagnosticsToken: "test-diagnostics-token"}}

	for name, testCase := range map[string]struct {
		provided string
		want     bool
	}{
		"missing": {want: false},
		"wrong":   {provided: "wrong", want: false},
		"valid":   {provided: "test-diagnostics-token", want: true},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if testCase.provided != "" {
				request.Header.Set("X-Limiter-Diagnostics-Token", testCase.provided)
			}
			if got := server.authorizeDiagnostics(recorder, request); got != testCase.want {
				t.Fatalf("authorized = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestDiagnosticsRateLimit(t *testing.T) {
	server := &Server{cfg: &config.Config{DiagnosticsToken: "test-diagnostics-token"}}
	for i := 0; i < diagnosticsRequestsPerMinute; i++ {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = "192.0.2.10:1234"
		request.Header.Set("X-Limiter-Diagnostics-Token", "test-diagnostics-token")
		if !server.authorizeDiagnostics(recorder, request) {
			t.Fatalf("request %d was unexpectedly rejected", i+1)
		}
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	request.Header.Set("X-Limiter-Diagnostics-Token", "test-diagnostics-token")
	if server.authorizeDiagnostics(recorder, request) {
		t.Fatal("request above the rate limit was accepted")
	}
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
}
