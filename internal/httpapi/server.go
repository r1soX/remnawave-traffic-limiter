package httpapi

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"remnawave-traffic-limiter/internal/config"
	"remnawave-traffic-limiter/internal/engine"
	"remnawave-traffic-limiter/internal/state"
	"remnawave-traffic-limiter/internal/webhook"
)

type Server struct {
	cfg                *config.Config
	store              *state.Store
	proc               *engine.Processor
	valid              *webhook.Validator
	locks              sync.Map
	diagnosticsMu      sync.Mutex
	diagnosticRequests map[string]requestWindow
	http               *http.Server
}

type requestWindow struct {
	started time.Time
	count   int
}

const diagnosticsRequestsPerMinute = 60

func New(cfg *config.Config, store *state.Store, proc *engine.Processor, valid *webhook.Validator) *Server {
	s := &Server{cfg: cfg, store: store, proc: proc, valid: valid, diagnosticRequests: make(map[string]requestWindow)}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)
	mux.HandleFunc(cfg.WebhookPath, s.handleWebhook)
	mux.HandleFunc(cfg.APIPath+"/state/", s.handleState)
	mux.HandleFunc(cfg.APIPath+"/reconcile/", s.handleReconcile)
	mux.HandleFunc(cfg.APIPath+"/pairing/", s.handlePairing)
	if cfg.SubscriptionGatewayEnabled {
		mux.HandleFunc("/sub/", s.handleSubscription)
	}

	s.http = &http.Server{Addr: ":" + cfg.Port, Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second}
	return s
}

type subscriptionResponse struct {
	body   []byte
	header http.Header
	status int
}

// handleSubscription is a capability URL gateway. It forwards ordinary
// subscriptions unchanged and combines a recorded Main/WhiteList pair in the
// format requested by the client: URI/Base64, Mihomo-family YAML, or JSON
// (sing-box/Xray). Unsupported opaque bodies are rejected rather than altered.
func (s *Server) handleSubscription(w http.ResponseWriter, r *http.Request) {
	shortUUID, clientType, ok := parseSubscriptionPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	main, err := s.proc.ResolveUser(shortUUID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	browserRequest := clientType == "" && acceptsHTML(r.Header)
	mainResponse, err := fetchSubscription(s.cfg.SubscriptionUpstreamURL, main.ShortUUID, clientType, r.Header)
	if err != nil {
		http.Error(w, "subscription upstream unavailable", http.StatusBadGateway)
		return
	}
	// A browser visit is the human-facing subscription page, not a client
	// update. Preserve it exactly as the original service serves it. The paired
	// config merger below is only for machine-readable subscription updates.
	if browserRequest {
		// The subscription-page frontend authenticates its subsequent
		// /assets/* and runtime-config requests with this session cookie.
		// Do not forward cookies for machine subscription updates below.
		writeSubscription(w, mainResponse, "", true)
		return
	}
	pair, err := s.store.GetPairByMainUserID(main.ID)
	if err == sql.ErrNoRows {
		writeSubscription(w, mainResponse, userSubscriptionInfo(main), false)
		return
	}
	if err != nil {
		http.Error(w, "subscription mapping unavailable", http.StatusInternalServerError)
		return
	}
	// A retained companion is deliberately DISABLED for Main-only and
	// WhiteList-only tariffs.  Never merge Remnawave's human-readable
	// "Subscription disabled" response into a client configuration.
	if !pair.Enabled {
		writeSubscription(w, mainResponse, userSubscriptionInfo(main), false)
		return
	}
	white, err := s.proc.Client.GetUserByID(pair.WhiteUserID)
	if err != nil {
		http.Error(w, "WhiteList user unavailable", http.StatusBadGateway)
		return
	}
	if strings.EqualFold(white.Status, "DISABLED") {
		writeSubscription(w, mainResponse, userSubscriptionInfo(main), false)
		return
	}
	whiteResponse, err := fetchSubscription(s.cfg.SubscriptionUpstreamURL, pair.WhiteShortUUID, clientType, r.Header)
	if err != nil {
		http.Error(w, "WhiteList subscription unavailable", http.StatusBadGateway)
		return
	}
	merged, err := mergeSubscriptions(mainResponse, whiteResponse)
	if err != nil {
		// Log only format metadata and the structural error. Subscription bodies
		// contain user credentials and must never appear in logs.
		slog.Warn(
			"subscription merge rejected",
			"main_user", main.ID,
			"main_content_type", mainResponse.header.Get("Content-Type"),
			"white_content_type", whiteResponse.header.Get("Content-Type"),
			"error", err,
		)
		http.Error(w, "subscription format is not supported by the paired gateway", http.StatusNotImplemented)
		return
	}
	mainResponse.body = merged
	writeSubscription(w, mainResponse, subscriptionUserInfo(
		int64(white.UserTraffic.UsedTrafficBytes),
		int64(white.TrafficLimitBytes),
		white.ExpireAt,
	), false)
}

func fetchSubscription(upstream, shortUUID, clientType string, incoming http.Header) (*subscriptionResponse, error) {
	target := strings.TrimRight(upstream, "/") + "/" + url.PathEscape(shortUUID)
	if clientType != "" {
		target += "/" + url.PathEscape(clientType)
	}
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	// Caddy can use this private routing marker to send gateway fetches to the
	// original subscription service and prevent a public /sub route loop.
	request.Header.Set("X-Remnawave-Limiter-Gateway", "1")
	// Remnawave selects a native subscription template by these headers. They
	// are safe to forward and must be identical for Main and WhiteList so both
	// responses use the same mergeable representation. Cookies and credentials
	// are intentionally never copied to an upstream request.
	for _, key := range []string{"User-Agent", "Accept", "Accept-Language"} {
		if value := incoming.Get(key); value != "" {
			request.Header.Set(key, value)
		}
	}
	// Device-identification headers still reach the upstream so native
	// HWID/device-limit validation remains effective.
	for _, key := range []string{"X-HWID", "X-Device-OS", "X-Ver-OS", "X-Device-Model"} {
		if value := incoming.Get(key); value != "" {
			request.Header.Set(key, value)
		}
	}
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream returned %s", response.Status)
	}
	return &subscriptionResponse{body: body, header: response.Header.Clone(), status: response.StatusCode}, nil
}

func acceptsHTML(headers http.Header) bool {
	return strings.Contains(strings.ToLower(headers.Get("Accept")), "text/html")
}

func subscriptionUserInfo(used, total int64, expireAt string) string {
	expire := int64(0)
	if parsed, err := time.Parse(time.RFC3339, expireAt); err == nil {
		expire = parsed.Unix()
	}
	if used < 0 {
		used = 0
	}
	if total < 0 {
		total = 0
	}
	return fmt.Sprintf("upload=0; download=%d; total=%d; expire=%d", used, total, expire)
}

// userSubscriptionInfo is intentionally calculated from the panel user we
// actually serve.  That makes a finite Main-only/WhiteList-only subscription
// display its native limit instead of inheriting an obsolete total=0 header.
func userSubscriptionInfo(user *engine.User) string {
	if user == nil {
		return ""
	}
	return subscriptionUserInfo(int64(user.UsedTrafficBytes), int64(user.TrafficLimitBytes), user.ExpireAt)
}

func writeSubscription(w http.ResponseWriter, response *subscriptionResponse, userInfo string, preserveBrowserCookies bool) {
	// Preserve subscription metadata added by the upstream (custom profile
	// headers, support URLs, update intervals, etc.).  Content-Length and
	// validators describe the original Main-only body and must not be relayed
	// after it is merged with the WhiteList response.
	for key, values := range response.header {
		if subscriptionHeaderMustBeRebuilt(key) && !(preserveBrowserCookies && strings.EqualFold(key, "set-cookie")) {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if userInfo != "" {
		w.Header().Set("Subscription-Userinfo", userInfo)
	}
	w.WriteHeader(response.status)
	_, _ = w.Write(response.body)
}

func subscriptionHeaderMustBeRebuilt(key string) bool {
	switch strings.ToLower(key) {
	case "content-length", "transfer-encoding", "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "upgrade", "etag", "last-modified", "accept-ranges", "set-cookie":
		return true
	default:
		return false
	}
}

func (s *Server) ListenAndServe(port string) error {
	if port == "" {
		port = s.cfg.Port
	}
	s.http.Addr = ":" + port
	return s.http.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Limit body size to prevent abuse
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB max

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Warn("webhook body read failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to read body"})
		return
	}

	if !s.valid.Validate(body, r.Header) {
		// Request headers are untrusted input and may contain credentials sent by
		// a proxy or client. Never copy them into application logs.
		slog.Warn("webhook signature invalid")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid signature"})
		return
	}

	event, err := webhook.ParseEvent(body)
	if err != nil {
		slog.Warn("webhook payload invalid", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid payload"})
		return
	}

	// Process async to prevent timeout
	go s.processWebhookEvent(event)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "accepted", "event": event.Event})
}

func (s *Server) processWebhookEvent(event *webhook.Event) {
	identifier := event.Data.Identifier()
	if identifier == "" {
		slog.Warn("webhook ignored: no resolvable user identifier", "event", event.Event)
		return
	}
	user, err := s.proc.ResolveUser(identifier)
	if err != nil {
		slog.Warn("user resolution failed", "identifier", identifier, "event", event.Event, "error", err)
		return
	}
	if user == nil {
		return
	}
	// The pairing control plane, periodic reconciler and all webhook variants
	// must serialize on the same Main id.  A shortUuid lock here used to let a
	// webhook caused by Main's PATCH race the tariff target and re-enable a
	// companion after the control endpoint had just disabled it.
	mainID := user.ID
	if pair, pairErr := s.store.GetPairByWhiteUserID(user.ID); pairErr == nil {
		mainID = pair.MainUserID
	}
	lockKey := "user-id:" + strconv.FormatInt(mainID, 10)
	actual, _ := s.locks.LoadOrStore(lockKey, &sync.Mutex{})
	mutex := actual.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()

	// The event may have waited behind a tariff transition. Reload rather than
	// reconciling an obsolete panel snapshot after that transition completed.
	user, err = s.proc.ResolveUser(identifier)
	if err != nil {
		slog.Warn("user resolution after paired lock failed", "identifier", identifier, "event", event.Event, "error", err)
		return
	}
	if user == nil {
		return
	}
	if s.proc.PairedWhiteListEnabled() {
		if !s.cfg.PairingAllowed(user.ShortUUID) {
			slog.Debug("paired WhiteList webhook skipped by pilot allowlist", "user", user.ID)
			return
		}
		if err := s.reconcileWhiteListUser(user); err != nil {
			slog.Error("WhiteList traffic reconciliation failed", "user", user.ID, "event", event.Event, "error", err)
			return
		}
	} else if err := s.proc.ApplyEvent(event.Event, user); err != nil {
		slog.Error("event processing failed", "user", user.ID, "event", event.Event, "error", err)
		return
	}
	if err := s.store.MarkEvent(identifier, event.Event); err != nil {
		slog.Warn("failed to save local event state", "user", identifier, "event", event.Event, "error", err)
	}
	slog.Info("webhook processed", "user", user.ID, "event", event.Event)
}

// ReconcileWhiteListUsers creates/synchronizes finite WhiteList pairs. It is
// invoked by the periodic runner and never needs Bedolaga identity fields.
func (s *Server) ReconcileWhiteListUsers() error {
	if !s.proc.PairedWhiteListEnabled() {
		return nil
	}
	users, err := s.proc.ListUsers()
	if err != nil {
		return err
	}
	var errs []error
	for _, user := range users {
		if !s.cfg.PairingAllowed(user.ShortUUID) {
			continue
		}
		// Technical companion accounts are returned by the panel user list as
		// well. They are reconciled through their Main account, never as a
		// second migration target; otherwise every periodic run performs the
		// same pair twice.
		if _, err := s.store.GetPairByWhiteUserID(user.ID); err == nil {
			continue
		} else if err != sql.ErrNoRows {
			errs = append(errs, fmt.Errorf("check technical user %d: %w", user.ID, err))
			continue
		}
		lockKey := "user-id:" + strconv.FormatInt(user.ID, 10)
		actual, _ := s.locks.LoadOrStore(lockKey, &sync.Mutex{})
		mutex := actual.(*sync.Mutex)
		mutex.Lock()
		err := s.reconcileWhiteListUser(user)
		mutex.Unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("user %d: %w", user.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Server) reconcileWhiteListUser(user *engine.User) error {
	result, err := s.proc.ReconcilePairedWhiteList(user)
	if err != nil {
		return err
	}
	identifier := user.ShortUUID
	if identifier == "" {
		identifier = strconv.FormatInt(user.ID, 10)
	}
	if result.State == state.StateBlocked {
		return s.store.SetBlocked(identifier)
	}
	return s.store.SetActive(identifier)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeDiagnostics(w, r) {
		return
	}

	identifier := strings.TrimPrefix(r.URL.Path, s.cfg.APIPath+"/state/")
	if identifier == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "user identifier required"})
		return
	}

	user, err := s.proc.ResolveUser(identifier)
	if err != nil {
		slog.Warn("user resolution failed", "identifier", identifier, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "user not found"})
		return
	}

	if user == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "user not found"})
		return
	}

	localState := state.StateActive
	if record, err := s.store.GetUserState(user.ShortUUID); err == nil && record.WhitelistState != "" {
		localState = record.WhitelistState
	}
	trafficLimit := user.TrafficLimitBytes
	trafficUsed := user.UsedTrafficBytes
	squads := user.ActiveInternalSquads
	var paired any
	if pair, err := s.store.GetPairByMainUserID(user.ID); err == nil && pair.Enabled {
		if white, whiteErr := s.proc.Client.GetUserByID(pair.WhiteUserID); whiteErr == nil {
			trafficLimit = white.TrafficLimitBytes
			trafficUsed = white.UserTraffic.UsedTrafficBytes
			localState = pair.State
			paired = map[string]any{"whiteUserId": pair.WhiteUserID, "whiteShortUuid": pair.WhiteShortUUID, "squads": white.ActiveInternalSquads.UUIDs()}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"user":       map[string]any{"uuid": user.ShortUUID, "shortUuid": user.ShortUUID, "status": user.Status},
		"traffic":    map[string]float64{"limitBytes": trafficLimit, "usedBytes": trafficUsed},
		"squads":     squads,
		"paired":     paired,
		"localState": localState,
	})
}

// authorizeDiagnostics protects endpoints that disclose panel identities or
// trigger a Remnawave reconciliation. Subscription links and signed webhooks
// intentionally use their own authorization model and do not call this helper.
func (s *Server) authorizeDiagnostics(w http.ResponseWriter, r *http.Request) bool {
	provided := r.Header.Get("X-Limiter-Diagnostics-Token")
	expected := s.cfg.DiagnosticsToken
	if len(provided) == len(expected) && len(expected) > 0 &&
		subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1 {
		if s.allowDiagnosticRequest(r.RemoteAddr) {
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "diagnostics rate limit exceeded"})
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid diagnostics token"})
	return false
}

func (s *Server) allowDiagnosticRequest(remoteAddr string) bool {
	client, _, err := net.SplitHostPort(remoteAddr)
	if err != nil || client == "" {
		client = remoteAddr
	}
	if client == "" {
		client = "unknown"
	}

	now := time.Now()
	s.diagnosticsMu.Lock()
	defer s.diagnosticsMu.Unlock()
	if s.diagnosticRequests == nil {
		s.diagnosticRequests = make(map[string]requestWindow)
	}
	for key, window := range s.diagnosticRequests {
		if now.Sub(window.started) >= 2*time.Minute {
			delete(s.diagnosticRequests, key)
		}
	}
	window := s.diagnosticRequests[client]
	if window.started.IsZero() || now.Sub(window.started) >= time.Minute {
		s.diagnosticRequests[client] = requestWindow{started: now, count: 1}
		return true
	}
	if window.count >= diagnosticsRequestsPerMinute {
		return false
	}
	window.count++
	s.diagnosticRequests[client] = window
	return true
}

// handlePairing is a small authenticated control plane used by Bedolaga after
// a tariff transition. It makes the transition immediate and retains the
// technical account on downgrade so a later LTE upgrade reuses it.
func (s *Server) handlePairing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.proc.PairedWhiteListEnabled() || s.cfg.PairedWhiteListControlToken == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "paired control is not configured"})
		return
	}
	provided := r.Header.Get("X-Paired-Whitelist-Token")
	if len(provided) != len(s.cfg.PairedWhiteListControlToken) || subtle.ConstantTimeCompare(
		[]byte(provided), []byte(s.cfg.PairedWhiteListControlToken),
	) != 1 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid paired control token"})
		return
	}
	identifier := strings.TrimPrefix(r.URL.Path, s.cfg.APIPath+"/pairing/")
	if identifier == "" || strings.Contains(identifier, "/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "user identifier required"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var request struct {
		Enabled              *bool     `json:"enabled"`
		TrafficLimitBytes    *int64    `json:"trafficLimitBytes"`
		TrafficLimitStrategy *string   `json:"trafficLimitStrategy"`
		ExpireAt             *string   `json:"expireAt"`
		Status               *string   `json:"status"`
		ActiveInternalSquads *[]string `json:"activeInternalSquads"`
		ResetWhiteTraffic    bool      `json:"resetWhiteTraffic"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Enabled == nil ||
		request.TrafficLimitBytes == nil || request.TrafficLimitStrategy == nil || request.ExpireAt == nil ||
		request.Status == nil || request.ActiveInternalSquads == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "full tariff target is required"})
		return
	}
	user, err := s.proc.ResolveUser(identifier)
	if err != nil || user == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "user not found"})
		return
	}
	if !s.cfg.PairingAllowed(user.ShortUUID) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "user is not in paired WhiteList allowlist"})
		return
	}
	lockKey := "user-id:" + strconv.FormatInt(user.ID, 10)
	actual, _ := s.locks.LoadOrStore(lockKey, &sync.Mutex{})
	mutex := actual.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	result, err := s.proc.ApplyPairedWhiteListIntent(user, engine.PairingIntent{
		Enabled:              *request.Enabled,
		TrafficLimitBytes:    request.TrafficLimitBytes,
		TrafficLimitStrategy: request.TrafficLimitStrategy,
		ExpireAt:             request.ExpireAt,
		Status:               request.Status,
		ActiveInternalSquads: request.ActiveInternalSquads,
		ResetWhiteTraffic:    request.ResetWhiteTraffic,
	})
	if err != nil {
		slog.Warn("paired tariff transition failed", "user", user.ID, "enabled", *request.Enabled, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "paired transition failed"})
		return
	}
	// Do not retain a stale BLOCKED diagnostic state after a switch back to a
	// Main-only tariff. Conversely, a 150 -> 50 GiB downgrade may immediately
	// exhaust WhiteList and must be reflected without waiting for polling.
	if result.State == state.StateBlocked {
		_ = s.store.SetBlocked(user.ShortUUID)
	} else {
		_ = s.store.SetActive(user.ShortUUID)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":            "ok",
		"user":              user.ShortUUID,
		"enabled":           *request.Enabled,
		"paired":            result.Paired,
		"trafficLimitBytes": result.TrafficLimit,
	})
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeDiagnostics(w, r) {
		return
	}

	identifier := strings.TrimPrefix(r.URL.Path, s.cfg.APIPath+"/reconcile/")
	if identifier == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "user identifier required"})
		return
	}

	user, err := s.proc.ResolveUser(identifier)
	if err != nil {
		slog.Warn("reconcile user resolution failed", "identifier", identifier, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "user not found"})
		return
	}

	if user == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "user not found"})
		return
	}

	var reconcileErr error
	if s.proc.PairedWhiteListEnabled() {
		if !s.cfg.PairingAllowed(user.ShortUUID) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "user is not in paired WhiteList allowlist"})
			return
		}
		mainID := user.ID
		if pair, pairErr := s.store.GetPairByWhiteUserID(user.ID); pairErr == nil {
			mainID = pair.MainUserID
		}
		lockKey := "user-id:" + strconv.FormatInt(mainID, 10)
		actual, _ := s.locks.LoadOrStore(lockKey, &sync.Mutex{})
		mutex := actual.(*sync.Mutex)
		mutex.Lock()
		reconcileErr = s.reconcileWhiteListUser(user)
		mutex.Unlock()
	} else {
		reconcileErr = s.proc.ApplyEvent("user.modified", user)
	}
	if reconcileErr != nil {
		slog.Warn("reconcile failed", "user", user.ID, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "reconciliation failed"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"user": user.ShortUUID, "status": "ok"})
}
