package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// Validator performs HMAC-SHA256 validation for Remnawave webhooks.
// The actual panel may sign with either a simple hex digest or a sha256= prefix,
// so we accept both common variants defensively.
type Validator struct{ secret string }

func NewValidator(secret string) (*Validator, error) {
	if secret == "" {
		return nil, fmt.Errorf("webhook secret is required")
	}
	return &Validator{secret: secret}, nil
}

func (v *Validator) Validate(body []byte, headers http.Header) bool {
	for _, key := range []string{"X-Remnawave-Signature", "X-Remnawave-Signature-256", "X-Signature", "X-Hub-Signature-256"} {
		value := strings.TrimSpace(headers.Get(key))
		if value == "" {
			continue
		}
		return validateSignature(v.secret, body, value)
	}
	return false
}

func validateSignature(secret string, body []byte, signature string) bool {
	signature = strings.TrimSpace(signature)
	signature = strings.TrimPrefix(signature, "sha256=")
	signature = strings.TrimPrefix(signature, "SHA256=")
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	returned, err := hex.DecodeString(signature)
	if err == nil {
		return hmac.Equal(returned, mac.Sum(nil))
	}
	return hmac.Equal([]byte(strings.ToLower(expected)), []byte(strings.ToLower(signature)))
}
