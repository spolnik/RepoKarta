package dependencies

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maximumCAFileBytes      = 16 << 20
	maximumCADirectoryFiles = 4096
)

// newDependencyHTTPClient preserves Go's proxy and connection defaults while
// making the OpenSSL-compatible CA environment portable to Windows. Go's Unix
// root loader already observes SSL_CERT_FILE and SSL_CERT_DIR, but the Windows
// system store does not, which otherwise makes registry and OSV calls fail only
// on TLS-inspected corporate machines.
func newDependencyHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	roots, err := dependencyRootCAs()
	if err != nil {
		return &http.Client{
			Timeout:   20 * time.Second,
			Transport: dependencyTransportError{err: err},
		}
	}
	if roots != nil {
		tlsConfig := transport.TLSClientConfig
		if transport.TLSClientConfig != nil {
			tlsConfig = transport.TLSClientConfig.Clone()
		} else {
			tlsConfig = new(tls.Config)
		}
		tlsConfig.RootCAs = roots
		transport.TLSClientConfig = tlsConfig
	}
	return &http.Client{Timeout: 20 * time.Second, Transport: transport}
}

func dependencyRootCAs() (*x509.CertPool, error) {
	certificateFile := strings.TrimSpace(os.Getenv("SSL_CERT_FILE"))
	certificateDirectories := strings.TrimSpace(os.Getenv("SSL_CERT_DIR"))
	if certificateFile == "" && certificateDirectories == "" {
		return nil, nil
	}

	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if certificateFile != "" {
		if err := appendCertificateFile(roots, certificateFile); err != nil {
			return nil, fmt.Errorf("load SSL_CERT_FILE: %w", err)
		}
	}
	if certificateDirectories != "" {
		for _, directory := range filepath.SplitList(certificateDirectories) {
			directory = strings.TrimSpace(directory)
			if directory == "" {
				continue
			}
			if err := appendCertificateDirectory(roots, directory); err != nil {
				return nil, fmt.Errorf("load SSL_CERT_DIR: %w", err)
			}
		}
	}
	return roots, nil
}

func appendCertificateDirectory(roots *x509.CertPool, directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) > maximumCADirectoryFiles {
		return fmt.Errorf(
			"directory contains %d entries; maximum is %d",
			len(entries),
			maximumCADirectoryFiles,
		)
	}
	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := appendCertificateFile(roots, filepath.Join(directory, entry.Name())); err != nil {
			if errors.Is(err, errNoCertificates) {
				continue
			}
			return fmt.Errorf("%s: %w", entry.Name(), err)
		}
		loaded++
	}
	if loaded == 0 {
		return errNoCertificates
	}
	return nil
}

var errNoCertificates = errors.New("no PEM certificates found")

func appendCertificateFile(roots *x509.CertPool, fileName string) error {
	file, err := os.Open(fileName)
	if err != nil {
		return err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maximumCAFileBytes+1))
	if err != nil {
		return err
	}
	if len(content) > maximumCAFileBytes {
		return fmt.Errorf("certificate bundle exceeds %d bytes", maximumCAFileBytes)
	}
	if !roots.AppendCertsFromPEM(content) {
		return errNoCertificates
	}
	return nil
}

type dependencyTransportError struct {
	err error
}

func (transport dependencyTransportError) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("configure dependency TLS trust: %w", transport.err)
}
