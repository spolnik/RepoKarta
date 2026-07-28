package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
)

func newConversationShareToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate conversation share token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func (s *Store) CreateConversationShare(
	ctx context.Context,
	conversationID string,
	authorID string,
) (agent.ConversationShare, error) {
	conversation, err := s.GetConversation(ctx, strings.TrimSpace(conversationID))
	if err != nil {
		return agent.ConversationShare{}, err
	}
	if conversation.Author.ID != strings.TrimSpace(authorID) {
		return agent.ConversationShare{}, agent.ErrConversationForbidden
	}
	token, err := newConversationShareToken()
	if err != nil {
		return agent.ConversationShare{}, err
	}
	share := agent.ConversationShare{
		Token: token, ConversationID: conversation.ID,
		AuthorID: conversation.Author.ID, CreatedAt: time.Now().UTC(),
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO conversation_shares(token, conversation_id, author_id, created_at, revoked_at)
VALUES (?, ?, ?, ?, '')
`, share.Token, share.ConversationID, share.AuthorID, formatTime(share.CreatedAt))
	if err != nil {
		return agent.ConversationShare{}, fmt.Errorf("create conversation share: %w", err)
	}
	return share, nil
}

func (s *Store) GetConversationShare(
	ctx context.Context,
	token string,
) (agent.ConversationShare, agent.Conversation, error) {
	var share agent.ConversationShare
	var created, revoked string
	err := s.db.QueryRowContext(ctx, `
SELECT token, conversation_id, author_id, created_at, revoked_at
FROM conversation_shares
WHERE token = ? AND revoked_at = ''
`, strings.TrimSpace(token)).Scan(
		&share.Token, &share.ConversationID, &share.AuthorID, &created, &revoked,
	)
	if err != nil {
		return share, agent.Conversation{}, agent.ErrConversationNotFound
	}
	share.CreatedAt = parseTime(created)
	share.RevokedAt = parseTime(revoked)
	conversation, err := s.GetConversation(ctx, share.ConversationID)
	return share, conversation, err
}

func (s *Store) RevokeConversationShare(
	ctx context.Context,
	token string,
	authorID string,
) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE conversation_shares
SET revoked_at = ?
WHERE token = ? AND author_id = ? AND revoked_at = ''
`, formatTime(time.Now().UTC()), strings.TrimSpace(token), strings.TrimSpace(authorID))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return agent.ErrConversationNotFound
	}
	return nil
}
