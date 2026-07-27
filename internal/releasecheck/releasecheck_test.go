// Package releasecheck keeps distribution policy executable without adding a
// runtime dependency to RepoKarta.
package releasecheck

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestWorkflowsAreValidYAMLAndUseCurrentNode24Actions(t *testing.T) {
	root := repositoryRoot(t)
	for _, relative := range []string{
		filepath.Join(".github", "workflows", "ci.yml"),
		filepath.Join(".github", "workflows", "release.yml"),
	} {
		content := readFile(t, root, relative)
		var document any
		if err := yaml.Unmarshal(content, &document); err != nil {
			t.Fatalf("%s is not valid YAML: %v", relative, err)
		}
		for _, expected := range []string{
			"actions/checkout@v7",
			"actions/setup-go@v6",
			"actions/setup-node@v6",
			`node-version: "24"`,
		} {
			if !strings.Contains(string(content), expected) {
				t.Fatalf("%s does not use %q", relative, expected)
			}
		}
	}
	release := string(readFile(t, root, filepath.Join(".github", "workflows", "release.yml")))
	for _, expected := range []string{
		"actions/upload-artifact@v7",
		"actions/download-artifact@v8",
		"sha256sum --check SHA256SUMS",
		"gh release create",
		"MACOS_CERTIFICATE_P12",
		"APPLE_APP_PASSWORD",
	} {
		if !strings.Contains(release, expected) {
			t.Fatalf("release workflow is missing %q", expected)
		}
	}
}

func TestEveryPackagePathCarriesRequiredLicensesAndVerification(t *testing.T) {
	root := repositoryRoot(t)
	required := []string{
		"zoekt-Apache-2.0.txt",
		"deps.dev-semver-Apache-2.0.txt",
		"gotreesitter-MIT.txt",
		"tree-sitter-grammars-MIT.txt",
		"nvim-treesitter-Kotlin-query-NOTICE.txt",
		"crewjam-saml-BSD-2-Clause.txt",
	}
	for _, relative := range []string{
		filepath.Join("scripts", "build.ps1"),
		filepath.Join("scripts", "build.sh"),
		filepath.Join("scripts", "package-release.ps1"),
		filepath.Join("scripts", "package-release.sh"),
		filepath.Join("Formula", "repokarta.rb"),
	} {
		content := string(readFile(t, root, relative))
		for _, license := range required {
			if !strings.Contains(content, license) &&
				!(relative == filepath.Join("Formula", "repokarta.rb") && license != "zoekt-Apache-2.0.txt") {
				t.Fatalf("%s does not package %s", relative, license)
			}
		}
	}
	powerShell := string(readFile(t, root, filepath.Join("scripts", "package-release.ps1")))
	for _, expected := range []string{"Get-FileHash", "Compress-Archive", "repokarta.exe", "main.version", "-buildvcs=false"} {
		if !strings.Contains(powerShell, expected) {
			t.Fatalf("Windows packager is missing %q", expected)
		}
	}
	shell := string(readFile(t, root, filepath.Join("scripts", "package-release.sh")))
	for _, expected := range []string{"shasum -a 256", "codesign --verify", "xcrun notarytool submit", "main.version", "-buildvcs=false"} {
		if !strings.Contains(shell, expected) {
			t.Fatalf("macOS packager is missing %q", expected)
		}
	}
	for _, script := range []string{powerShell, shell} {
		for _, expected := range []string{"shared-deployment.md", "enterprise-administration.md", "repokarta.env.example"} {
			if !strings.Contains(script, expected) {
				t.Fatalf("release packager is missing shared operations artifact %q", expected)
			}
		}
	}
}

func TestHomebrewCIUsesRegisteredTap(t *testing.T) {
	root := repositoryRoot(t)
	workflow := string(readFile(t, root, filepath.Join(".github", "workflows", "ci.yml")))
	for _, expected := range []string{
		"brew tap spolnik/repokarta https://github.com/spolnik/RepoKarta.git",
		"brew install --HEAD --build-from-source spolnik/repokarta/repokarta",
		"brew test spolnik/repokarta/repokarta",
	} {
		if !strings.Contains(workflow, expected) {
			t.Fatalf("Homebrew CI is missing %q", expected)
		}
	}
	if strings.Contains(workflow, "brew install --HEAD --build-from-source ./Formula/") {
		t.Fatal("Homebrew CI attempts to install a formula outside a registered tap")
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve releasecheck source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
}

func readFile(t *testing.T, root, relative string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return content
}
