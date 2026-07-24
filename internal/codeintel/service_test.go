package codeintel

import "testing"

func TestSourceWindowKeepsFocusInsideUsefulContext(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		start      int
		end        int
		windowFrom int
		windowTo   int
	}{
		{name: "near start", start: 7, end: 7, windowFrom: 1, windowTo: 200},
		{name: "middle", start: 300, end: 300, windowFrom: 220, windowTo: 419},
		{name: "wide bounded range", start: 100, end: 700, windowFrom: 201, windowTo: 700},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			start, end := SourceWindow(testCase.start, testCase.end)
			if start != testCase.windowFrom || end != testCase.windowTo {
				t.Fatalf("SourceWindow(%d, %d) = %d-%d, want %d-%d", testCase.start, testCase.end, start, end, testCase.windowFrom, testCase.windowTo)
			}
			if end-start+1 > MaximumSourceLines {
				t.Fatalf("window has %d lines, maximum is %d", end-start+1, MaximumSourceLines)
			}
		})
	}
}
