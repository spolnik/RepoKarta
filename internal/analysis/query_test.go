package analysis

import (
	"slices"
	"testing"
)

func TestStructuralQueryJavaCapturesAnnotationAndAncestor(t *testing.T) {
	source := []byte(`package example;

class Controller {
  @GetMapping
  public String list() {
    return "ok";
  }

  public String helper() {
    return "hidden";
  }
}
`)
	query := `((method_declaration
  (modifiers
    (marker_annotation
      name: (identifier) @annotation))
  name: (identifier) @method) @declaration
  (#eq? @annotation "GetMapping")
  (#has-ancestor? @method class_declaration))`
	compiled, err := CompileStructuralQuery("java", query)
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Execute(source, QueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Truncated || len(result.Matches) != 1 {
		t.Fatalf("result = %#v, want one complete match", result)
	}
	captures := result.Matches[0].Captures
	if !slices.ContainsFunc(captures, func(capture QueryCapture) bool {
		return capture.Name == "annotation" && capture.Text == "GetMapping"
	}) {
		t.Fatalf("captures = %#v, want GetMapping annotation", captures)
	}
	if !slices.ContainsFunc(captures, func(capture QueryCapture) bool {
		return capture.Name == "method" && capture.Text == "list" && capture.Range.StartLine == 5
	}) {
		t.Fatalf("captures = %#v, want list method on line 5", captures)
	}
}

func TestStructuralQueryGoCapturesCallWithRelationalPredicate(t *testing.T) {
	source := []byte(`package api

func route(router *Router) {
	router.Handle("/ready")
}

var outside = router.Handle("/outside")
`)
	query := `((call_expression
  function: (selector_expression
    field: (field_identifier) @method)) @call
  (#eq? @method "Handle")
  (#has-ancestor? @call function_declaration))`
	compiled, err := CompileStructuralQuery("go", query)
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Execute(source, QueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Truncated || len(result.Matches) != 1 {
		t.Fatalf("result = %#v, want one complete match", result)
	}
	if !slices.ContainsFunc(result.Matches[0].Captures, func(capture QueryCapture) bool {
		return capture.Name == "method" && capture.Text == "Handle"
	}) {
		t.Fatalf("captures = %#v, want Handle method", result.Matches[0].Captures)
	}
}

func TestStructuralQueryReportsMatchLimit(t *testing.T) {
	compiled, err := CompileStructuralQuery("go", `(identifier) @identifier`)
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Execute(
		[]byte("package p\nvar one, two, three = 1, 2, 3\n"),
		QueryOptions{MatchLimit: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 2 || !result.Truncated {
		t.Fatalf("result = %#v, want two truncated matches", result)
	}
}

func TestStructuralQueryRequiresNamedCapture(t *testing.T) {
	if _, err := CompileStructuralQuery("go", `(function_declaration)`); err == nil {
		t.Fatal("CompileStructuralQuery() accepted a query without a named capture")
	}
}

func TestRequiredRootKindsIsConservative(t *testing.T) {
	query := `; declarations
((method_declaration name: (identifier) @name) @method
 (#match? @name "^get"))
(class_declaration name: (identifier) @class)`
	if got, want := RequiredRootKinds(query), []string{"class_declaration", "method_declaration"}; !slices.Equal(got, want) {
		t.Fatalf("RequiredRootKinds() = %#v, want %#v", got, want)
	}
	for _, unsafe := range []string{
		`[(method_declaration) (constructor_declaration)] @callable`,
		`(_) @node`,
		`"public" @keyword`,
		`(expression/binary_expression) @binary`,
	} {
		if got := RequiredRootKinds(unsafe); len(got) != 0 {
			t.Fatalf("RequiredRootKinds(%q) = %#v, want no prefilter", unsafe, got)
		}
	}
}

func BenchmarkStructuralQueryJava(b *testing.B) {
	source := []byte(`package example;
class Controller {
  @GetMapping public String list() { return service.list(); }
  @PostMapping public String save() { return service.save(); }
  public String helper() { return "ok"; }
}`)
	compiled, err := CompileStructuralQuery("java", `(method_declaration name: (identifier) @method) @declaration`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		result, executeErr := compiled.Execute(source, QueryOptions{})
		if executeErr != nil || len(result.Matches) != 3 {
			b.Fatalf("Execute() = %#v, %v", result, executeErr)
		}
	}
}
