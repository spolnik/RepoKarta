package security

import (
	"context"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSAMLMiddlewarePersistsIdentityAndServesMetadata(t *testing.T) {
	t.Parallel()
	idp := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/samlmetadata+xml")
		_, _ = response.Write([]byte(`<?xml version="1.0"?>
<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.example.com">
  <IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example.com/sso"/>
  </IDPSSODescriptor>
</EntityDescriptor>`))
	}))
	defer idp.Close()
	dataDirectory := t.TempDir()
	settings := Settings{
		Mode:            ModeSAML,
		PublicURL:       "https://repo.example.com",
		SAMLMetadataURL: idp.URL,
	}
	middleware, err := buildSAMLMiddleware(context.Background(), settings, dataDirectory, idp.Client())
	if err != nil {
		t.Fatal(err)
	}
	if middleware.ServiceProvider.EntityID != "https://repo.example.com/saml/metadata" {
		t.Fatalf("entity ID = %q", middleware.ServiceProvider.EntityID)
	}
	for _, name := range []string{"saml-key.pem", "saml-cert.pem"} {
		if _, err := os.Stat(filepath.Join(dataDirectory, "security", name)); err != nil {
			t.Fatalf("%s was not persisted: %v", name, err)
		}
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "https://repo.example.com/saml/metadata", nil)
	middleware.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "AssertionConsumerService") {
		t.Fatalf("metadata response = %d, %q", recorder.Code, recorder.Body.String())
	}

	second, err := buildSAMLMiddleware(context.Background(), settings, dataDirectory, idp.Client())
	if err != nil {
		t.Fatal(err)
	}
	if !middleware.ServiceProvider.Key.Public().(*rsa.PublicKey).Equal(second.ServiceProvider.Key.Public()) {
		t.Fatal("SAML identity changed across reload")
	}
}
