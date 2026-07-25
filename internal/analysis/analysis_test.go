package analysis

import (
	"slices"
	"testing"
)

func TestAnalyzePriorityLanguages(t *testing.T) {
	tests := []struct {
		path       string
		source     string
		wantLang   string
		wantSymbol string
		wantKind   string
	}{
		{
			path:       "src/main/java/com/acme/PaymentService.java",
			source:     `package com.acme; public class PaymentService { public void charge() {} }`,
			wantLang:   "java",
			wantSymbol: "PaymentService",
			wantKind:   "class",
		},
		{
			path:       "src/main/kotlin/com/acme/PaymentService.kt",
			source:     `package com.acme; class PaymentService { fun charge() = Unit }`,
			wantLang:   "kotlin",
			wantSymbol: "PaymentService",
			wantKind:   "class",
		},
		{
			path:       "src/payment.ts",
			source:     `export interface Payment { id: string }; export function charge(): void {}`,
			wantLang:   "typescript",
			wantSymbol: "Payment",
			wantKind:   "interface",
		},
		{
			path:       "src/payment.js",
			source:     `export class PaymentService { charge() {} }`,
			wantLang:   "javascript",
			wantSymbol: "PaymentService",
			wantKind:   "class",
		},
		{
			path:       "internal/payment/payment.go",
			source:     `package payment; func Charge() {}`,
			wantLang:   "go",
			wantSymbol: "Charge",
			wantKind:   "function",
		},
		{
			path:       "migrations/001.sql",
			source:     `CREATE TABLE payments (id bigint PRIMARY KEY);`,
			wantLang:   "sql",
			wantSymbol: "payments",
			wantKind:   "table",
		},
		{
			path:       "scripts/release.sh",
			source:     "release() { echo ok; }\n",
			wantLang:   "bash",
			wantSymbol: "release",
			wantKind:   "function",
		},
		{
			path:       "tools/report.py",
			source:     "def report():\n    return True\n",
			wantLang:   "python",
			wantSymbol: "report",
			wantKind:   "function",
		},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			document, supported := Analyze(test.path, []byte(test.source))
			if !supported {
				t.Fatal("file was not recognized")
			}
			if document.Language != test.wantLang {
				t.Fatalf("language = %q, want %q", document.Language, test.wantLang)
			}
			if !document.ParseComplete {
				t.Fatalf("parse incomplete: %#v", document.Diagnostics)
			}
			if !slices.ContainsFunc(document.Symbols, func(symbol Symbol) bool {
				return symbol.Name == test.wantSymbol &&
					symbol.Kind == test.wantKind &&
					symbol.Confidence == "syntax" &&
					symbol.Range.StartLine > 0 &&
					symbol.Range.EndByte > symbol.Range.StartByte
			}) {
				t.Fatalf("symbols = %#v, want %s %s", document.Symbols, test.wantKind, test.wantSymbol)
			}
		})
	}
}

func TestAnalyzeExtractsImportsCallsAndHeritage(t *testing.T) {
	document, supported := Analyze(
		"src/main/java/com/acme/PaymentService.java",
		[]byte(`package com.acme;
import com.acme.store.PaymentStore;
public class PaymentService extends BaseService implements Chargeable {
    private final PaymentStore store;
    public void charge() { store.save(); }
}`),
	)
	if !supported || !document.ParseComplete {
		t.Fatalf("analysis = supported %v, document %#v", supported, document)
	}
	for _, expected := range []struct {
		kind   string
		target string
	}{
		{kind: "import", target: "import com.acme.store.PaymentStore;"},
		{kind: "call", target: "save"},
		{kind: "extends", target: "BaseService"},
		{kind: "implements", target: "Chargeable"},
	} {
		if !slices.ContainsFunc(document.Relations, func(relation Relation) bool {
			return relation.Kind == expected.kind &&
				relation.Target == expected.target &&
				relation.Confidence == "syntax" &&
				relation.Range.StartLine > 0
		}) {
			t.Fatalf("relations = %#v, want %s %s", document.Relations, expected.kind, expected.target)
		}
	}
}

func TestAnalyzeGradleGroovyAndKotlinBuildFacts(t *testing.T) {
	tests := []struct {
		path   string
		source string
	}{
		{
			path: "build.gradle",
			source: `plugins { id 'java' }
dependencies {
    implementation 'org.springframework.boot:spring-boot-starter-web:4.0.1'
    testImplementation project(':contract-tests')
}`,
		},
		{
			path: "build.gradle.kts",
			source: `plugins { id("org.jetbrains.kotlin.jvm") version "2.2.0" }
dependencies {
    implementation("org.springframework.boot:spring-boot-starter-web:4.0.1")
}`,
		},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			document, supported := Analyze(test.path, []byte(test.source))
			if !supported {
				t.Fatal("file was not recognized")
			}
			if !document.ParseComplete {
				t.Fatalf("parse incomplete: %#v", document.Diagnostics)
			}
			if !slices.ContainsFunc(document.BuildFacts, func(fact BuildFact) bool {
				return fact.Kind == "plugin"
			}) {
				t.Fatalf("build facts have no plugin: %#v", document.BuildFacts)
			}
			if !slices.ContainsFunc(document.BuildFacts, func(fact BuildFact) bool {
				return fact.Kind == "dependency"
			}) {
				t.Fatalf("build facts have no dependency: %#v", document.BuildFacts)
			}
		})
	}
}

func TestAnalyzeKeepsPartialFactsOnSyntaxError(t *testing.T) {
	document, supported := Analyze(
		"Broken.java",
		[]byte("public class Usable { public void usable() {} public void broken( {"),
	)
	if !supported {
		t.Fatal("file was not recognized")
	}
	if document.ParseComplete {
		t.Fatal("broken source was reported complete")
	}
	if len(document.Diagnostics) == 0 {
		t.Fatal("partial parse did not explain the recovery")
	}
	if !slices.ContainsFunc(document.Symbols, func(symbol Symbol) bool {
		return symbol.Name == "usable"
	}) {
		t.Fatalf("partial symbols = %#v", document.Symbols)
	}
}

func TestLanguageForPathRejectsUnsupportedFiles(t *testing.T) {
	if got := LanguageForPath("README.md"); got != "" {
		t.Fatalf("language = %q, want unsupported", got)
	}
}
