package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/apicontract"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/security"
)

func (s *Server) listConversations(response http.ResponseWriter, request *http.Request) {
	viewer := s.conversationViewer(request.Context())
	conversations, err := s.history.ListConversations(request.Context(), agent.ConversationFilter{
		AuthorID: viewer.Author.ID,
	})
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, err)
		return
	}
	if conversations == nil {
		conversations = []agent.Conversation{}
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"conversations": conversations,
		"viewer":        viewer.Author,
		"can_view_all":  false,
		"scope":         "own",
	})
}

func (s *Server) getConversation(response http.ResponseWriter, request *http.Request) {
	conversation, err := s.history.GetConversation(
		request.Context(),
		strings.TrimSpace(request.PathValue("conversationID")),
	)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	if err := s.authorizeConversation(request.Context(), conversation); err != nil {
		s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.read", "denied")
		writeConversationError(response, err)
		return
	}
	s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.read", "success")
	writeJSON(response, http.StatusOK, conversation)
}

func (s *Server) renameConversation(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 4<<10)
	var input struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid conversation title"))
		return
	}
	conversationID := strings.TrimSpace(request.PathValue("conversationID"))
	conversation, err := s.history.GetConversation(request.Context(), conversationID)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	if err := s.authorizeConversation(request.Context(), conversation); err != nil {
		s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.rename", "denied")
		writeConversationError(response, err)
		return
	}
	s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.rename", "success")
	if err := s.history.RenameConversation(
		request.Context(),
		conversationID,
		input.Title,
	); err != nil {
		writeConversationError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteConversation(response http.ResponseWriter, request *http.Request) {
	conversationID := strings.TrimSpace(request.PathValue("conversationID"))
	conversation, err := s.history.GetConversation(request.Context(), conversationID)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	if err := s.authorizeConversation(request.Context(), conversation); err != nil {
		s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.delete", "denied")
		writeConversationError(response, err)
		return
	}
	s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.delete", "success")
	if err := s.history.DeleteConversation(
		request.Context(),
		conversationID,
	); err != nil {
		writeConversationError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func writeConversationError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agent.ErrConversationNotFound), errors.Is(err, sql.ErrNoRows):
		writeAPIError(response, http.StatusNotFound, errors.New("conversation not found"))
	case errors.Is(err, agent.ErrConversationForbidden):
		writeAPIError(response, http.StatusForbidden, err)
	case errors.Is(err, agent.ErrInvalidInput):
		writeAPIError(response, http.StatusBadRequest, err)
	default:
		writeAPIError(response, http.StatusInternalServerError, err)
	}
}

func (s *Server) providerStatuses(response http.ResponseWriter, request *http.Request) {
	writeJSON(response, http.StatusOK, apicontract.ProviderStatusesResponse{
		Providers: s.agents.Statuses(request.Context()),
	})
}

func (s *Server) chat(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, maximumChatRequestBytes)
	var turn agent.TurnRequest
	if err := json.NewDecoder(request.Body).Decode(&turn); err != nil {
		http.Error(response, "Invalid conversation request", http.StatusBadRequest)
		return
	}
	turn.Message = strings.TrimSpace(turn.Message)
	turn.Provider = strings.TrimSpace(turn.Provider)
	turn.Model = strings.TrimSpace(turn.Model)
	turn.Effort = strings.TrimSpace(turn.Effort)
	turn.ConversationID = strings.TrimSpace(turn.ConversationID)
	effective, err := s.intelligence.ResolveEffectiveContexts(
		request.Context(),
		contextscope.EffectiveRequest{
			Contexts:        turn.ContextSelectors,
			NamedContextIDs: turn.NamedContextIDs,
			UseDefaults:     turn.UseDefaultContexts,
		},
	)
	if err != nil {
		writeContextOrAPIError(response, err)
		return
	}
	turn.Contexts = effective.Contexts
	viewer := s.conversationViewer(request.Context())
	turn.Author = viewer.Author
	for index := range turn.Images {
		turn.Images[index].Name = strings.TrimSpace(turn.Images[index].Name)
		turn.Images[index].MediaType = strings.ToLower(strings.TrimSpace(turn.Images[index].MediaType))
	}
	if (turn.Message == "" && len(turn.Images) == 0) || (turn.ConversationID == "" && turn.Provider == "") {
		http.Error(response, "Provider and message or image are required", http.StatusBadRequest)
		return
	}
	if err := agent.ValidateImages(turn.Images); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}

	flusher, ok := response.(http.Flusher)
	if !ok {
		http.Error(response, "Streaming is not supported", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Accel-Buffering", "no")
	encoder := json.NewEncoder(response)
	emit := func(event agent.Event) error {
		if err := encoder.Encode(event); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if err := s.agents.Send(request.Context(), turn, emit); err != nil {
		slog.Warn("conversation turn failed", "provider", turn.Provider, "error", err)
		_ = emit(agent.Event{Type: agent.EventError, ConversationID: turn.ConversationID, Text: err.Error()})
	}
}

func (s *Server) retryChat(response http.ResponseWriter, request *http.Request) {
	retrier, ok := s.agents.(ConversationRetryService)
	if !ok || s.history == nil {
		writeAPIError(response, http.StatusServiceUnavailable, errors.New("conversation retry is unavailable"))
		return
	}
	var input struct {
		Strategy       string `json:"strategy"`
		TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
		TokenBudget    int64  `json:"token_budget,omitempty"`
		ToolCallBudget int    `json:"tool_call_budget,omitempty"`
	}
	if err := decodeBoundedJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid retry request"))
		return
	}
	conversationID := strings.TrimSpace(request.PathValue("conversationID"))
	conversation, err := s.history.GetConversation(request.Context(), conversationID)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	if err := s.authorizeConversation(request.Context(), conversation); err != nil {
		writeConversationError(response, err)
		return
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		writeAPIError(response, http.StatusInternalServerError, errors.New("streaming is not supported"))
		return
	}
	response.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Accel-Buffering", "no")
	encoder := json.NewEncoder(response)
	emit := func(event agent.Event) error {
		if err := encoder.Encode(event); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	viewer := s.conversationViewer(request.Context())
	err = retrier.Retry(request.Context(), agent.RetryRequest{
		ConversationID: conversationID,
		Author:         viewer.Author,
		Strategy:       input.Strategy,
		TimeoutSeconds: input.TimeoutSeconds,
		TokenBudget:    input.TokenBudget,
		ToolCallBudget: input.ToolCallBudget,
	}, emit)
	if err != nil {
		_ = emit(agent.Event{Type: agent.EventError, ConversationID: conversationID, Text: err.Error()})
	}
}

func (s *Server) shareConversation(response http.ResponseWriter, request *http.Request) {
	conversationID := strings.TrimSpace(request.PathValue("conversationID"))
	conversation, err := s.history.GetConversation(request.Context(), conversationID)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	if err := s.authorizeConversation(request.Context(), conversation); err != nil {
		writeConversationError(response, err)
		return
	}
	if conversation.Mode != "deep_search" {
		writeAPIError(response, http.StatusUnprocessableEntity, errors.New("only Deep Search conversations can be shared"))
		return
	}
	viewer := s.conversationViewer(request.Context())
	share, err := s.conversationShares.CreateConversationShare(
		request.Context(), conversation.ID, viewer.Author.ID,
	)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, map[string]any{
		"share": share,
		"url":   "/api/shared/deep/" + url.PathEscape(share.Token),
	})
}

func (s *Server) revokeConversationShare(response http.ResponseWriter, request *http.Request) {
	viewer := s.conversationViewer(request.Context())
	if err := s.conversationShares.RevokeConversationShare(
		request.Context(), request.PathValue("token"), viewer.Author.ID,
	); err != nil {
		writeConversationError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) sharedDeepSearch(response http.ResponseWriter, request *http.Request) {
	share, conversation, err := s.conversationShares.GetConversationShare(
		request.Context(), request.PathValue("token"),
	)
	if err != nil || conversation.Mode != "deep_search" {
		writeAPIError(response, http.StatusNotFound, agent.ErrConversationNotFound)
		return
	}
	if err := s.revalidateConversationSources(request.Context(), conversation); err != nil {
		writeAPIError(
			response,
			http.StatusForbidden,
			errors.New("shared Deep Search contains a source that is not visible to the current viewer"),
		)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"share": map[string]any{
			"token":      share.Token,
			"created_at": share.CreatedAt,
		},
		"conversation": map[string]any{
			"id":            conversation.ID,
			"title":         conversation.Title,
			"mode":          conversation.Mode,
			"provider":      conversation.Provider,
			"model":         conversation.Model,
			"created_at":    conversation.CreatedAt,
			"updated_at":    conversation.UpdatedAt,
			"input_tokens":  conversation.InputTokens,
			"output_tokens": conversation.OutputTokens,
			"messages":      conversation.Messages,
		},
		"permission_revalidated": true,
	})
}

func (s *Server) revalidateConversationSources(
	ctx context.Context,
	conversation agent.Conversation,
) error {
	checked := make(map[int64]struct{})
	check := func(repositoryID int64) error {
		if repositoryID <= 0 {
			return nil
		}
		if _, ok := checked[repositoryID]; ok {
			return nil
		}
		if _, err := s.intelligence.RepositoryByID(ctx, repositoryID); err != nil {
			return err
		}
		checked[repositoryID] = struct{}{}
		return nil
	}
	for _, message := range conversation.Messages {
		for _, structured := range message.Contexts {
			if err := check(structured.RepositoryID); err != nil {
				return err
			}
		}
		for _, citation := range message.Sources {
			repositoryID, ok := sourceRepositoryID(citation.URL)
			if ok {
				if err := check(repositoryID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func sourceRepositoryID(raw string) (int64, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for index := 0; index+1 < len(segments); index++ {
		switch segments[index] {
		case "source", "projects", "wiki":
		default:
			continue
		}
		id, err := strconv.ParseInt(segments[index+1], 10, 64)
		if err == nil && id > 0 {
			return id, true
		}
	}
	for _, key := range []string{"repo", "repository_id"} {
		id, err := strconv.ParseInt(parsed.Query().Get(key), 10, 64)
		if err == nil && id > 0 {
			return id, true
		}
	}
	return 0, false
}

func (s *Server) interruptChat(response http.ResponseWriter, request *http.Request) {
	conversationID := strings.TrimSpace(request.PathValue("conversationID"))
	if conversationID == "" {
		http.Error(response, "Conversation is required", http.StatusBadRequest)
		return
	}
	if s.history == nil {
		writeAPIError(response, http.StatusServiceUnavailable, errors.New("conversation authorization is unavailable"))
		return
	}
	conversation, err := s.history.GetConversation(request.Context(), conversationID)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	if err := s.authorizeConversation(request.Context(), conversation); err != nil {
		s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.interrupt", "denied")
		writeConversationError(response, err)
		return
	}
	s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.interrupt", "success")
	if err := s.agents.Interrupt(request.Context(), conversationID); err != nil {
		switch {
		case errors.Is(err, agent.ErrConversationNotFound):
			http.Error(response, err.Error(), http.StatusNotFound)
		case errors.Is(err, agent.ErrNoActiveTurn):
			http.Error(response, err.Error(), http.StatusConflict)
		default:
			slog.Warn("interrupt conversation turn", "conversation_id", conversationID, "error", err)
			http.Error(response, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) conversationViewer(ctx context.Context) conversationViewer {
	principal, ok := security.PrincipalFromContext(ctx)
	if !ok {
		return conversationViewer{
			Author: agent.ConversationAuthor{
				ID:       "local:admin",
				Name:     "Local administrator",
				Provider: string(security.ModeLocal),
			},
			Admin: true,
		}
	}
	provider := strings.TrimSpace(principal.Provider)
	if provider == "" {
		provider = "authenticated"
	}
	identity := strings.TrimSpace(principal.ID)
	if identity == "" {
		identity = strings.ToLower(strings.TrimSpace(principal.Email))
	}
	if identity == "" {
		identity = "anonymous"
	}
	name := strings.TrimSpace(principal.Name)
	if name == "" {
		name = strings.TrimSpace(principal.Email)
	}
	if name == "" {
		name = identity
	}
	return conversationViewer{
		Author: agent.ConversationAuthor{
			ID:       provider + ":" + identity,
			Name:     name,
			Email:    strings.TrimSpace(principal.Email),
			Provider: provider,
			Groups:   append([]string(nil), principal.Groups...),
		},
		Admin: principal.Admin,
	}
}

func (s *Server) authorizeConversation(ctx context.Context, conversation agent.Conversation) error {
	viewer := s.conversationViewer(ctx)
	if conversation.Author.ID == viewer.Author.ID {
		return nil
	}
	return agent.ErrConversationForbidden
}
