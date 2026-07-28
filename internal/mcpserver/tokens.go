package mcpserver

import (
	"crypto/sha256"
	"sync"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
)

const conversationTokenLifetime = 2 * time.Hour

type conversationGrant struct {
	ConversationID string
	ActorID        string
	ExpiresAt      time.Time
}

// TokenAuthority issues independently revocable credentials for provider
// sessions. Raw tokens are never retained.
type TokenAuthority struct {
	mu             sync.Mutex
	grants         map[[32]byte]conversationGrant
	byConversation map[string]map[[32]byte]struct{}
	now            func() time.Time
}

func NewTokenAuthority() *TokenAuthority {
	return &TokenAuthority{
		grants:         make(map[[32]byte]conversationGrant),
		byConversation: make(map[string]map[[32]byte]struct{}),
		now:            time.Now,
	}
}

// Issue implements agent.MCPTokenIssuer.
func (a *TokenAuthority) Issue(conversationID string, author agent.ConversationAuthor) (string, error) {
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte(token))
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked(a.now())
	a.revokeLocked(conversationID)
	a.grants[key] = conversationGrant{
		ConversationID: conversationID,
		ActorID:        author.ID,
		ExpiresAt:      a.now().Add(conversationTokenLifetime),
	}
	a.byConversation[conversationID] = map[[32]byte]struct{}{key: {}}
	return token, nil
}

// Validate confirms that the credential was issued for this exact
// conversation and has not expired or been revoked.
func (a *TokenAuthority) Validate(token, conversationID string) bool {
	if a == nil || token == "" || conversationID == "" {
		return false
	}
	key := sha256.Sum256([]byte(token))
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked(now)
	grant, ok := a.grants[key]
	return ok && grant.ConversationID == conversationID && grant.ExpiresAt.After(now)
}

// Revoke invalidates every credential issued for one conversation.
func (a *TokenAuthority) Revoke(conversationID string) {
	if a == nil || conversationID == "" {
		return
	}
	a.mu.Lock()
	a.revokeLocked(conversationID)
	a.mu.Unlock()
}

func (a *TokenAuthority) revokeLocked(conversationID string) {
	for key := range a.byConversation[conversationID] {
		delete(a.grants, key)
	}
	delete(a.byConversation, conversationID)
}

func (a *TokenAuthority) pruneLocked(now time.Time) {
	for key, grant := range a.grants {
		if grant.ExpiresAt.After(now) {
			continue
		}
		delete(a.grants, key)
		if keys := a.byConversation[grant.ConversationID]; keys != nil {
			delete(keys, key)
			if len(keys) == 0 {
				delete(a.byConversation, grant.ConversationID)
			}
		}
	}
}
