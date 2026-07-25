package security

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCloudflareValidatorAcceptsSignedApplicationToken(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "current-key"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/cdn-cgi/access/certs" {
			http.NotFound(response, request)
			return
		}
		exponent := big.NewInt(int64(key.PublicKey.E)).Bytes()
		_ = json.NewEncoder(response).Encode(cloudflareJWKS{Keys: []cloudflareJWK{{
			Algorithm: "RS256",
			Exponent:  base64.RawURLEncoding.EncodeToString(exponent),
			KeyID:     keyID,
			Modulus:   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			KeyType:   "RSA",
			Use:       "sig",
		}}})
	}))
	defer server.Close()

	now := time.Now()
	token := signedJWT(t, key, keyID, map[string]any{
		"aud":   []string{"other", "repo-audience"},
		"email": "developer@example.com",
		"exp":   now.Add(5 * time.Minute).Unix(),
		"iat":   now.Add(-time.Minute).Unix(),
		"iss":   server.URL,
		"name":  "Developer",
		"sub":   "user-123",
	})
	validator := NewCloudflareValidator(server.URL, "repo-audience", server.Client())
	principal, err := validator.Validate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.ID != "user-123" || principal.Email != "developer@example.com" || principal.Provider != string(ModeCloudflareAccess) {
		t.Fatalf("principal = %#v", principal)
	}
}

func TestCloudflareValidatorRejectsWrongAudienceAndSignature(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(cloudflareJWKS{Keys: []cloudflareJWK{{
			Algorithm: "RS256",
			Exponent:  "AQAB",
			KeyID:     "key",
			Modulus:   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			KeyType:   "RSA",
		}}})
	}))
	defer server.Close()
	now := time.Now()
	validator := NewCloudflareValidator(server.URL, "expected", server.Client())
	wrongAudience := signedJWT(t, key, "key", map[string]any{
		"aud": "wrong",
		"exp": now.Add(time.Minute).Unix(),
		"iss": server.URL,
		"sub": "user",
	})
	if _, err := validator.Validate(context.Background(), wrongAudience); err == nil {
		t.Fatal("Validate() accepted the wrong audience")
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	wrongSignature := signedJWT(t, otherKey, "key", map[string]any{
		"aud": "expected",
		"exp": now.Add(time.Minute).Unix(),
		"iss": server.URL,
		"sub": "user",
	})
	if _, err := validator.Validate(context.Background(), wrongSignature); err == nil {
		t.Fatal("Validate() accepted an untrusted signature")
	}
}

func signedJWT(t *testing.T, key *rsa.PrivateKey, keyID string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": keyID, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := crypto.SHA256.New()
	_, _ = digest.Write([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}
