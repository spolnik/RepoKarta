package mcpserver

import (
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
)

func TestConversationTokensAreScopedRevocableAndExpiring(t *testing.T) {
	authority := NewTokenAuthority()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	authority.now = func() time.Time { return now }
	token, err := authority.Issue("conversation-a", agent.ConversationAuthor{ID: "saml:alice"})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.Validate(token, "conversation-a") {
		t.Fatal("issued token was not accepted for its conversation")
	}
	if authority.Validate(token, "conversation-b") {
		t.Fatal("conversation token crossed its authorization boundary")
	}
	authority.Revoke("conversation-a")
	if authority.Validate(token, "conversation-a") {
		t.Fatal("revoked token remained valid")
	}
	token, err = authority.Issue("conversation-a", agent.ConversationAuthor{ID: "saml:alice"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(conversationTokenLifetime + time.Second)
	if authority.Validate(token, "conversation-a") {
		t.Fatal("expired token remained valid")
	}
}
