package access

import (
	"context"
	"reflect"
	"testing"
)

func TestViewerContextAndIdentityNormalization(t *testing.T) {
	if _, ok := ViewerFromContext(context.Background()); ok {
		t.Fatal("empty context unexpectedly contained a viewer")
	}
	ctx := WithViewer(context.Background(), Viewer{
		ID: " alice ", Groups: []string{" Team ", "team", "", "Ops"}, Admin: true,
	})
	viewer, ok := ViewerFromContext(ctx)
	if !ok || viewer.ID != "alice" || !viewer.Admin ||
		!reflect.DeepEqual(viewer.Groups, []string{"Team", "Ops"}) {
		t.Fatalf("viewer = %#v, present = %v", viewer, ok)
	}
	for _, testCase := range []struct {
		provider string
		identity string
		want     string
	}{
		{"local", "admin", "local:admin"},
		{"", "", "authenticated:anonymous"},
		{" saml ", " alice ", "saml:alice"},
	} {
		if got := IdentityID(testCase.provider, testCase.identity); got != testCase.want {
			t.Fatalf("IdentityID(%q, %q) = %q", testCase.provider, testCase.identity, got)
		}
	}
}
