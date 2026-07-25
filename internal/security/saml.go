package security

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crewjam/saml/samlsp"
)

const samlKeyBits = 3072

func buildSAMLMiddleware(ctx context.Context, settings Settings, dataDirectory string, client *http.Client) (*samlsp.Middleware, error) {
	publicURL, err := url.Parse(settings.PublicURL + "/")
	if err != nil {
		return nil, fmt.Errorf("parse public URL: %w", err)
	}
	metadataURL, err := url.Parse(settings.SAMLMetadataURL)
	if err != nil {
		return nil, fmt.Errorf("parse SAML metadata URL: %w", err)
	}
	metadata, err := samlsp.FetchMetadata(ctx, client, *metadataURL)
	if err != nil {
		return nil, fmt.Errorf("fetch SAML IdP metadata: %w", err)
	}
	key, certificate, err := loadOrCreateSAMLIdentity(dataDirectory, publicURL.Hostname())
	if err != nil {
		return nil, err
	}
	entityID := settings.SAMLEntityID
	if entityID == "" {
		entityID = publicURL.ResolveReference(&url.URL{Path: "saml/metadata"}).String()
	}
	middleware, err := samlsp.New(samlsp.Options{
		URL:                *publicURL,
		EntityID:           entityID,
		Key:                key,
		Certificate:        certificate,
		HTTPClient:         client,
		IDPMetadata:        metadata,
		AllowIDPInitiated:  false,
		DefaultRedirectURI: "/",
		SignRequest:        false,
		CookieName:         "repokarta_saml_session",
		CookieSameSite:     http.SameSiteLaxMode,
	})
	if err != nil {
		return nil, fmt.Errorf("create SAML service provider: %w", err)
	}
	middleware.OnError = func(response http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(response, "SAML authentication failed", http.StatusUnauthorized)
	}
	return middleware, nil
}

func loadOrCreateSAMLIdentity(dataDirectory, hostname string) (*rsa.PrivateKey, *x509.Certificate, error) {
	if strings.TrimSpace(dataDirectory) == "" {
		return nil, nil, errors.New("data directory is required for SAML key storage")
	}
	securityDirectory := filepath.Join(dataDirectory, "security")
	keyPath := filepath.Join(securityDirectory, "saml-key.pem")
	certificatePath := filepath.Join(securityDirectory, "saml-cert.pem")
	key, keyErr := readSAMLKey(keyPath)
	certificate, certificateErr := readSAMLCertificate(certificatePath)
	if keyErr == nil && certificateErr == nil {
		if key.PublicKey.Equal(certificate.PublicKey) {
			return key, certificate, nil
		}
		return nil, nil, errors.New("stored SAML key does not match its certificate")
	}
	if !errors.Is(keyErr, os.ErrNotExist) || !errors.Is(certificateErr, os.ErrNotExist) {
		if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
			return nil, nil, keyErr
		}
		if certificateErr != nil && !errors.Is(certificateErr, os.ErrNotExist) {
			return nil, nil, certificateErr
		}
		return nil, nil, errors.New("stored SAML key pair is incomplete")
	}
	if err := os.MkdirAll(securityDirectory, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create SAML security directory: %w", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, samlKeyBits)
	if err != nil {
		return nil, nil, fmt.Errorf("generate SAML private key: %w", err)
	}
	now := time.Now()
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("generate SAML certificate serial: %w", err)
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   hostname,
			Organization: []string{"RepoKarta"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create SAML certificate: %w", err)
	}
	certificate, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse generated SAML certificate: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write SAML private key: %w", err)
	}
	if err := os.WriteFile(certificatePath, certificatePEM, 0o644); err != nil {
		return nil, nil, fmt.Errorf("write SAML certificate: %w", err)
	}
	return key, certificate, nil
}

func readSAMLKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		return nil, errors.New("stored SAML private key is not valid PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse stored SAML private key: %w", err)
	}
	return key, nil
}

func readSAMLCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("stored SAML certificate is not valid PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse stored SAML certificate: %w", err)
	}
	return certificate, nil
}

func principalFromSAML(session samlsp.Session) (Principal, bool) {
	withAttributes, ok := session.(samlsp.SessionWithAttributes)
	if !ok {
		return Principal{}, false
	}
	attributes := withAttributes.GetAttributes()
	first := func(names ...string) string {
		for _, name := range names {
			if value := strings.TrimSpace(attributes.Get(name)); value != "" {
				return value
			}
		}
		return ""
	}
	email := first(
		"email",
		"mail",
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
	)
	identifier := first(
		"nameid",
		"NameID",
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/nameidentifier",
	)
	if identifier == "" {
		identifier = email
	}
	if identifier == "" {
		return Principal{}, false
	}
	groups := attributes["groups"]
	if len(groups) == 0 {
		groups = attributes["http://schemas.xmlsoap.org/claims/Group"]
	}
	return Principal{
		ID:       identifier,
		Email:    email,
		Name:     first("name", "displayName", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name"),
		Provider: string(ModeSAML),
		Groups:   append([]string(nil), groups...),
	}, true
}
