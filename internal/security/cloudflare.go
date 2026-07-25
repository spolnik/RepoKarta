package security

import (
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const cloudflareKeyLifetime = 5 * time.Minute

type jwtAudience []string

func (audience *jwtAudience) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*audience = []string{single}
		return nil
	}
	var multiple []string
	if err := json.Unmarshal(data, &multiple); err != nil {
		return errors.New("JWT audience must be a string or string array")
	}
	*audience = multiple
	return nil
}

type cloudflareClaims struct {
	Audience  jwtAudience `json:"aud"`
	Email     string      `json:"email"`
	Expires   int64       `json:"exp"`
	Groups    []string    `json:"groups"`
	IssuedAt  int64       `json:"iat"`
	Issuer    string      `json:"iss"`
	Name      string      `json:"name"`
	NotBefore int64       `json:"nbf"`
	Subject   string      `json:"sub"`
}

type cloudflareJWK struct {
	Algorithm string `json:"alg"`
	Exponent  string `json:"e"`
	KeyID     string `json:"kid"`
	Modulus   string `json:"n"`
	KeyType   string `json:"kty"`
	Use       string `json:"use"`
}

type cloudflareJWKS struct {
	Keys []cloudflareJWK `json:"keys"`
}

// CloudflareValidator verifies Cloudflare Access application tokens locally.
type CloudflareValidator struct {
	teamURL  *url.URL
	audience string
	client   *http.Client

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	expiresAt time.Time
}

func NewCloudflareValidator(teamDomain, audience string, client *http.Client) *CloudflareValidator {
	teamURL, _ := url.Parse(strings.TrimRight(teamDomain, "/"))
	return &CloudflareValidator{
		teamURL:  teamURL,
		audience: audience,
		client:   client,
		keys:     make(map[string]*rsa.PublicKey),
	}
}

func (validator *CloudflareValidator) Validate(ctx context.Context, token string) (Principal, error) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return Principal{}, errors.New("invalid Cloudflare Access token")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		return Principal{}, errors.New("invalid Cloudflare Access token header")
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return Principal{}, errors.New("Cloudflare Access token must use RS256 and include a key ID")
	}

	key, err := validator.key(ctx, header.KeyID)
	if err != nil {
		return Principal{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil {
		return Principal{}, errors.New("invalid Cloudflare Access token signature")
	}
	digest := crypto.SHA256.New()
	_, _ = digest.Write([]byte(segments[0] + "." + segments[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest.Sum(nil), signature); err != nil {
		return Principal{}, errors.New("Cloudflare Access token signature is not trusted")
	}

	claimsBytes, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return Principal{}, errors.New("invalid Cloudflare Access token claims")
	}
	var claims cloudflareClaims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return Principal{}, errors.New("invalid Cloudflare Access token claims")
	}
	now := time.Now()
	issuer := strings.TrimRight(validator.teamURL.String(), "/")
	if strings.TrimRight(claims.Issuer, "/") != issuer {
		return Principal{}, errors.New("Cloudflare Access token has an unexpected issuer")
	}
	audienceOK := false
	for _, candidate := range claims.Audience {
		if candidate == validator.audience {
			audienceOK = true
			break
		}
	}
	if !audienceOK {
		return Principal{}, errors.New("Cloudflare Access token has an unexpected audience")
	}
	const clockSkew = 2 * time.Minute
	if claims.Expires == 0 || now.After(time.Unix(claims.Expires, 0).Add(clockSkew)) {
		return Principal{}, errors.New("Cloudflare Access token has expired")
	}
	if claims.NotBefore != 0 && now.Add(clockSkew).Before(time.Unix(claims.NotBefore, 0)) {
		return Principal{}, errors.New("Cloudflare Access token is not valid yet")
	}
	if claims.IssuedAt != 0 && now.Add(clockSkew).Before(time.Unix(claims.IssuedAt, 0)) {
		return Principal{}, errors.New("Cloudflare Access token was issued in the future")
	}
	identity := strings.TrimSpace(claims.Email)
	if identity == "" {
		identity = strings.TrimSpace(claims.Subject)
	}
	if identity == "" {
		return Principal{}, errors.New("Cloudflare Access token does not identify a user")
	}
	return Principal{
		ID:       claims.Subject,
		Email:    claims.Email,
		Name:     claims.Name,
		Provider: string(ModeCloudflareAccess),
		Groups:   claims.Groups,
	}, nil
}

func (validator *CloudflareValidator) key(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	validator.mu.RLock()
	key := validator.keys[keyID]
	fresh := time.Now().Before(validator.expiresAt)
	validator.mu.RUnlock()
	if fresh {
		if key == nil {
			return nil, errors.New("Cloudflare Access signing key was not found")
		}
		return key, nil
	}
	if err := validator.refresh(ctx); err != nil {
		return nil, err
	}
	validator.mu.RLock()
	defer validator.mu.RUnlock()
	key = validator.keys[keyID]
	if key == nil {
		return nil, errors.New("Cloudflare Access signing key was not found")
	}
	return key, nil
}

func (validator *CloudflareValidator) refresh(ctx context.Context) error {
	keysURL := validator.teamURL.ResolveReference(&url.URL{Path: "/cdn-cgi/access/certs"})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, keysURL.String(), nil)
	if err != nil {
		return err
	}
	response, err := validator.client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch Cloudflare Access signing keys: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch Cloudflare Access signing keys: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read Cloudflare Access signing keys: %w", err)
	}
	var set cloudflareJWKS
	if err := json.Unmarshal(body, &set); err != nil {
		return fmt.Errorf("decode Cloudflare Access signing keys: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, jwk := range set.Keys {
		if jwk.KeyID == "" || jwk.KeyType != "RSA" ||
			(jwk.Algorithm != "" && jwk.Algorithm != "RS256") ||
			(jwk.Use != "" && jwk.Use != "sig") {
			continue
		}
		modulus, err := base64.RawURLEncoding.DecodeString(jwk.Modulus)
		if err != nil {
			continue
		}
		exponentBytes, err := base64.RawURLEncoding.DecodeString(jwk.Exponent)
		if err != nil || len(exponentBytes) == 0 || len(exponentBytes) > 4 {
			continue
		}
		exponent := 0
		for _, part := range exponentBytes {
			exponent = exponent<<8 | int(part)
		}
		if exponent < 3 {
			continue
		}
		keys[jwk.KeyID] = &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: exponent}
	}
	if len(keys) == 0 {
		return errors.New("Cloudflare Access returned no usable RSA signing keys")
	}
	lifetime := cloudflareKeyLifetime
	if maxAge := cacheMaxAge(response.Header.Get("Cache-Control")); maxAge > 0 && maxAge < lifetime {
		lifetime = maxAge
	}
	validator.mu.Lock()
	validator.keys = keys
	validator.expiresAt = time.Now().Add(lifetime)
	validator.mu.Unlock()
	return nil
}

func cacheMaxAge(value string) time.Duration {
	for _, directive := range strings.Split(value, ",") {
		name, raw, found := strings.Cut(strings.TrimSpace(directive), "=")
		if found && strings.EqualFold(name, "max-age") {
			seconds, err := strconv.Atoi(strings.TrimSpace(raw))
			if err == nil && seconds > 0 {
				return time.Duration(seconds) * time.Second
			}
		}
	}
	return 0
}
