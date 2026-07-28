package codeintel

import "testing"

func TestPinnedSavedSearchRequiresExactRevisionScope(t *testing.T) {
	if _, err := validSavedSearch(SavedSearchInput{
		Title: "Unpinned", RevisionPolicy: "pinned",
		Request: SearchRequest{Query: "PaymentService"},
	}); err == nil {
		t.Fatal("pinned search without a revision unexpectedly succeeded")
	}
	if _, err := validSavedSearch(SavedSearchInput{
		Title: "Pinned", RevisionPolicy: "pinned",
		Request: SearchRequest{Query: "PaymentService revision:abcdef"},
	}); err != nil {
		t.Fatalf("pinned revision search failed: %v", err)
	}
}

func TestMonitorRevisionScopesMustContainTheSameRepositories(t *testing.T) {
	if !revisionScopesComparable("1:aaa,2:bbb", "1:ccc,2:ddd") {
		t.Fatal("same repository scope was not comparable")
	}
	if revisionScopesComparable("1:aaa,2:bbb", "1:ccc,3:ddd") {
		t.Fatal("changed repository scope was comparable")
	}
}
