package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"testing"
)

func TestValidateRequestSignature(t *testing.T) {
	secret := "super-secret"
	body := []byte(`{"scope":"user","event":"user.limited"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	signature := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest("POST", "/webhook", nil)
	req.Header.Set("X-Remnawave-Signature", signature)
	v, err := NewValidator(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Validate(body, req.Header) {
		t.Fatal("expected valid signature")
	}

	req.Header.Set("X-Remnawave-Signature", "deadbeef")
	if v.Validate(body, req.Header) {
		t.Fatal("expected invalid signature")
	}
}
