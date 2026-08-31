package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	serviceURL    = "http://localhost:8080"
	webhookSecret = "test-secret"
)

func isServiceRunning() bool {
	conn, err := net.DialTimeout("tcp", "localhost:8080", 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func skipIfServiceNotRunning(t *testing.T) {
	if !isServiceRunning() {
		t.Skip("Service not running. Start with: docker-compose up")
	}
}

type TestPayload struct {
	Scope     string `json:"scope"`
	Event     string `json:"event"`
	Timestamp string `json:"timestamp"`
	Data      struct {
		ID                   int64   `json:"id"`
		ShortUUID            string  `json:"shortUuid"`
		Username             string  `json:"username"`
		Status               string  `json:"status"`
		TrafficLimitBytes    float64 `json:"trafficLimitBytes"`
		TrafficLimitStrategy string  `json:"trafficLimitStrategy"`
		ActiveInternalSquads []struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		} `json:"activeInternalSquads"`
	} `json:"data"`
	Meta map[string]interface{} `json:"meta"`
}

func generateSignature(payload []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	return fmt.Sprintf("sha256=%s", hex.EncodeToString(h.Sum(nil)))
}

func TestHealthEndpoint(t *testing.T) {
	skipIfServiceNotRunning(t)

	resp, err := http.Get(serviceURL + "/health")
	if err != nil {
		t.Fatalf("Health check failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "ok" {
		t.Errorf("Expected status 'ok', got %s", result["status"])
	}
}

func TestReadyEndpoint(t *testing.T) {
	skipIfServiceNotRunning(t)

	resp, err := http.Get(serviceURL + "/ready")
	if err != nil {
		t.Fatalf("Readiness check failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
}

func TestWebhookWithValidSignature(t *testing.T) {
	skipIfServiceNotRunning(t)

	payload := TestPayload{
		Scope:     "user",
		Event:     "user.limited",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data: struct {
			ID                   int64   `json:"id"`
			ShortUUID            string  `json:"shortUuid"`
			Username             string  `json:"username"`
			Status               string  `json:"status"`
			TrafficLimitBytes    float64 `json:"trafficLimitBytes"`
			TrafficLimitStrategy string  `json:"trafficLimitStrategy"`
			ActiveInternalSquads []struct {
				UUID string `json:"uuid"`
				Name string `json:"name"`
			} `json:"activeInternalSquads"`
		}{
			ID:                   123,
			ShortUUID:            "abc123",
			Username:             "test@example.com",
			Status:               "ACTIVE",
			TrafficLimitBytes:    1073741824,
			TrafficLimitStrategy: "daily",
		},
		Meta: make(map[string]interface{}),
	}

	payloadBytes, _ := json.Marshal(payload)
	signature := generateSignature(payloadBytes, webhookSecret)

	req, _ := http.NewRequest(
		http.MethodPost,
		serviceURL+"/webhook",
		bytes.NewReader(payloadBytes),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature", signature)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Webhook request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d. Body: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "accepted" {
		t.Errorf("Expected status 'accepted', got %v", result["status"])
	}
}

func TestWebhookWithInvalidSignature(t *testing.T) {
	skipIfServiceNotRunning(t)

	payload := TestPayload{
		Scope: "user",
		Event: "user.limited",
	}

	payloadBytes, _ := json.Marshal(payload)

	req, _ := http.NewRequest(
		http.MethodPost,
		serviceURL+"/webhook",
		bytes.NewReader(payloadBytes),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature", "sha256=invalid")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Webhook request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", resp.StatusCode)
	}
}

func TestStateEndpoint(t *testing.T) {
	skipIfServiceNotRunning(t)

	resp, err := http.Get(serviceURL + "/api/state/test-user")
	if err != nil {
		t.Fatalf("State request failed: %v", err)
	}
	defer resp.Body.Close()

	// Should be 404 if user doesn't exist, or 200 if found
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 200 or 404, got %d", resp.StatusCode)
	}

	if resp.StatusCode == http.StatusOK {
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		if _, ok := result["user"]; !ok {
			t.Errorf("Expected 'user' field in response")
		}
	}
}

func TestReconcileEndpoint(t *testing.T) {
	skipIfServiceNotRunning(t)

	req, _ := http.NewRequest(
		http.MethodPost,
		serviceURL+"/api/reconcile/test-user",
		nil,
	)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Reconcile request failed: %v", err)
	}
	defer resp.Body.Close()

	// Should be 404 if user doesn't exist, or 200 if found
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 200 or 404, got %d", resp.StatusCode)
	}
}

func TestWebhookEventProcessing(t *testing.T) {
	skipIfServiceNotRunning(t)

	events := []string{
		"user.limited",
		"user.traffic_reset",
		"user.modified",
	}

	for _, event := range events {
		t.Run(event, func(t *testing.T) {
			payload := TestPayload{
				Scope:     "user",
				Event:     event,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Data: struct {
					ID                   int64   `json:"id"`
					ShortUUID            string  `json:"shortUuid"`
					Username             string  `json:"username"`
					Status               string  `json:"status"`
					TrafficLimitBytes    float64 `json:"trafficLimitBytes"`
					TrafficLimitStrategy string  `json:"trafficLimitStrategy"`
					ActiveInternalSquads []struct {
						UUID string `json:"uuid"`
						Name string `json:"name"`
					} `json:"activeInternalSquads"`
				}{
					ID:        456,
					ShortUUID: fmt.Sprintf("test-%s", event),
					Username:  fmt.Sprintf("user-%s@example.com", strings.ReplaceAll(event, ".", "-")),
					Status:    "ACTIVE",
				},
				Meta: make(map[string]interface{}),
			}

			payloadBytes, _ := json.Marshal(payload)
			signature := generateSignature(payloadBytes, webhookSecret)

			req, _ := http.NewRequest(
				http.MethodPost,
				serviceURL+"/webhook",
				bytes.NewReader(payloadBytes),
			)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Hub-Signature", signature)

			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Webhook request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("Expected status 200 for %s, got %d. Body: %s", event, resp.StatusCode, string(body))
			}
		})
	}
}

// Run tests manually in CI/CD with:
// docker-compose up -d  # Start service first
// SERVICE_URL=http://localhost:8080 WEBHOOK_SECRET=test-secret go test ./cmd/integration_test -v
