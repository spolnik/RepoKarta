package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/apicontract"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codework"
)

func (s *Server) codePage(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)
		return
	}
	viewer := s.conversationViewer(request.Context())
	sessions, err := s.code.List(request.Context(), viewer.Author.ID)
	if err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)
		return
	}
	var providers []agent.Status
	for _, status := range s.agents.Statuses(request.Context()) {
		if status.Code && status.Available && status.Authenticated {
			providers = append(providers, status)
		}
	}
	var repositories []catalog.Repository
	for _, repository := range data.Repositories {
		enabled, enabledErr := s.code.RepositoryEnabled(request.Context(), repository.ID)
		if enabledErr != nil {
			http.Error(response, enabledErr.Error(), http.StatusInternalServerError)
			return
		}
		if enabled {
			repositories = append(repositories, repository)
		}
	}
	data.ActivePage = "code"
	s.render(response, "code", codePageData{
		pageData: data, Sessions: sessions, Providers: providers,
		CodeRepositories: repositories,
	})
}

func (s *Server) codeSessions(response http.ResponseWriter, request *http.Request) {
	viewer := s.conversationViewer(request.Context())
	sessions, err := s.code.List(request.Context(), viewer.Author.ID)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	if sessions == nil {
		sessions = []codework.Session{}
	}
	writeJSON(response, http.StatusOK, apicontract.CodeSessionsResponse{Sessions: sessions})
}

func (s *Server) createCodeSession(response http.ResponseWriter, request *http.Request) {
	var input struct {
		RepositoryID int64  `json:"repository_id"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		Effort       string `json:"effort"`
		Baseline     string `json:"baseline"`
	}
	if err := decodeBoundedJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid Code session request"))
		return
	}
	input.Provider = strings.TrimSpace(input.Provider)
	input.Model = strings.TrimSpace(input.Model)
	input.Effort = strings.TrimSpace(input.Effort)
	if err := s.validateCodeProvider(request.Context(), input.Provider, input.Model, input.Effort); err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	viewer := s.conversationViewer(request.Context())
	session, err := s.code.Create(request.Context(), codework.CreateSessionRequest{
		RepositoryID: input.RepositoryID,
		AuthorID:     viewer.Author.ID,
		Provider:     input.Provider,
		Model:        input.Model,
		Effort:       input.Effort,
		Baseline:     strings.TrimSpace(input.Baseline),
	})
	if err != nil {
		writeCodeError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, session)
}

func (s *Server) validateCodeProvider(ctx context.Context, provider, model, effort string) error {
	for _, status := range s.agents.Statuses(ctx) {
		if status.ID != provider {
			continue
		}
		if !status.Code || !status.Available || !status.Authenticated {
			return errors.New("selected coding provider is unavailable")
		}
		if model == "" {
			if effort != "" {
				return errors.New("a coding model is required when reasoning effort is selected")
			}
			return nil
		}
		for _, candidate := range status.Models {
			if candidate.ID != model {
				continue
			}
			if effort == "" {
				return nil
			}
			for _, supported := range candidate.Efforts {
				if effort == supported {
					return nil
				}
			}
			return errors.New("selected reasoning effort is not supported by this model")
		}
		return errors.New("selected coding model is not supported by this provider")
	}
	return errors.New("selected coding provider was not found")
}

func (s *Server) codeSession(response http.ResponseWriter, request *http.Request) {
	sessionID, authorID := s.codeIdentity(request)
	session, err := s.code.Session(request.Context(), sessionID, authorID)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	actions, err := s.code.Actions(request.Context(), sessionID, authorID)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	if actions == nil {
		actions = []codework.Action{}
	}
	approvals, err := s.code.Approvals(request.Context(), sessionID, authorID)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	if approvals == nil {
		approvals = []codework.Approval{}
	}
	var diff *codework.Diff
	if session.State != codework.StateDiscarded {
		current, diffErr := s.code.Diff(request.Context(), sessionID, authorID, codework.DefaultContextLines)
		if diffErr == nil {
			diff = &current
		}
	}
	var conversation *agent.Conversation
	if session.ConversationID != "" && s.history != nil {
		stored, conversationErr := s.history.GetConversation(request.Context(), session.ConversationID)
		if conversationErr == nil && stored.Author.ID == authorID && stored.Code {
			conversation = &stored
		}
	}
	writeJSON(response, http.StatusOK, apicontract.CodeSessionDetailResponse{
		Session: session, Conversation: conversation, Actions: actions,
		Approvals: approvals, Diff: diff,
	})
}

func (s *Server) codeDiff(response http.ResponseWriter, request *http.Request) {
	sessionID, authorID := s.codeIdentity(request)
	contextLines, _ := strconv.Atoi(request.URL.Query().Get("context"))
	diff, err := s.code.Diff(request.Context(), sessionID, authorID, contextLines)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, diff)
}

func (s *Server) codeFile(response http.ResponseWriter, request *http.Request) {
	sessionID, authorID := s.codeIdentity(request)
	file, err := s.code.ReadFile(
		request.Context(), sessionID, authorID, request.URL.Query().Get("path"),
	)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, file)
}

func (s *Server) codeTurn(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Message        string `json:"message"`
		TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
		TokenBudget    int64  `json:"token_budget,omitempty"`
	}
	request.Body = http.MaxBytesReader(response, request.Body, maximumChatRequestBytes)
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid Code turn request"))
		return
	}
	input.Message = strings.TrimSpace(input.Message)
	if input.Message == "" {
		writeAPIError(response, http.StatusBadRequest, errors.New("Code message is required"))
		return
	}
	sessionID, authorID := s.codeIdentity(request)
	session, err := s.code.Session(request.Context(), sessionID, authorID)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	switch session.State {
	case codework.StateFinished, codework.StateDiscarded, codework.StateDiscarding,
		codework.StateFinishing:
		writeAPIError(response, http.StatusConflict, errors.New("Code session is not editable"))
		return
	}
	workspace, err := s.code.WorkspacePath(request.Context(), sessionID, authorID)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	viewer := s.conversationViewer(request.Context())
	turn := agent.TurnRequest{
		ConversationID: session.ConversationID,
		Provider:       session.Provider,
		Model:          session.Model,
		Effort:         session.Effort,
		Message:        input.Message,
		TimeoutSeconds: input.TimeoutSeconds,
		TokenBudget:    input.TokenBudget,
		Author:         viewer.Author,
		Mode:           "chat",
		Coding:         true,
		WorkspaceRoot:  workspace,
	}
	if _, err := s.code.SetState(
		request.Context(), sessionID, authorID, codework.StateRunning, "",
	); err != nil {
		writeCodeError(response, err)
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
		if event.Type == agent.EventMeta && event.ConversationID != "" &&
			session.ConversationID == "" {
			attached, attachErr := s.code.AttachConversation(
				request.Context(), sessionID, authorID, event.ConversationID,
			)
			if attachErr != nil {
				return attachErr
			}
			session = attached
		}
		if event.Type == agent.EventCodeAction && event.CodeAction != nil {
			_, _ = s.code.RecordAction(request.Context(), codework.Action{
				SessionID: sessionID,
				Kind:      event.CodeAction.Kind,
				Status:    event.CodeAction.Status,
				Summary:   event.CodeAction.Summary,
				Command:   event.CodeAction.Command,
				Output:    event.CodeAction.Output,
				ExitCode:  event.CodeAction.ExitCode,
			})
		}
		if event.Type == agent.EventApproval && event.Approval != nil {
			if err := s.code.RecordApproval(request.Context(), codework.Approval{
				ID:        event.Approval.ID,
				SessionID: sessionID,
				Kind:      event.Approval.Kind,
				Reason:    event.Approval.Reason,
				Command:   event.Approval.Command,
				Directory: event.Approval.Directory,
			}); err != nil {
				return err
			}
			_, _ = s.code.SetState(
				request.Context(), sessionID, authorID,
				codework.StateAwaitingApproval, "",
			)
		}
		if err := encoder.Encode(event); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	sendErr := s.agents.Send(request.Context(), turn, emit)
	finalState := codework.StateReady
	detail := ""
	if errors.Is(sendErr, agent.ErrInterrupted) {
		finalState = codework.StateInterrupted
	} else if sendErr != nil {
		finalState = codework.StateFailed
		detail = sendErr.Error()
	}
	_, _ = s.code.SetState(request.Context(), sessionID, authorID, finalState, detail)
	if diff, diffErr := s.code.Diff(
		contextWithoutCancellation(request), sessionID, authorID, codework.DefaultContextLines,
	); diffErr == nil {
		_, _ = s.code.RecordAction(contextWithoutCancellation(request), codework.Action{
			SessionID: sessionID, Kind: "diff_snapshot", Status: "complete",
			Summary: strconv.Itoa(diff.FilesChanged) + " changed file(s)",
		})
	}
	if sendErr != nil {
		slog.Warn("Code turn failed", "session", sessionID, "provider", session.Provider, "error", sendErr)
		_ = emit(agent.Event{Type: agent.EventError, ConversationID: session.ConversationID, Text: sendErr.Error()})
	}
}

func (s *Server) interruptCodeTurn(response http.ResponseWriter, request *http.Request) {
	sessionID, authorID := s.codeIdentity(request)
	session, err := s.code.Session(request.Context(), sessionID, authorID)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	if session.ConversationID == "" {
		writeAPIError(response, http.StatusConflict, errors.New("Code session has no active conversation"))
		return
	}
	if err := s.agents.Interrupt(request.Context(), session.ConversationID); err != nil {
		writeCodeError(response, err)
		return
	}
	_, _ = s.code.SetState(
		request.Context(), sessionID, authorID, codework.StateInterrupted, "",
	)
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) resolveCodeApproval(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Decision string `json:"decision"`
	}
	if err := decodeBoundedJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid Code approval"))
		return
	}
	sessionID, authorID := s.codeIdentity(request)
	session, err := s.code.Session(request.Context(), sessionID, authorID)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	approvals, ok := s.agents.(ConversationApprovalService)
	if !ok || session.ConversationID == "" {
		writeAPIError(response, http.StatusServiceUnavailable, errors.New("provider approvals are unavailable"))
		return
	}
	approvalID := strings.TrimSpace(request.PathValue("approvalID"))
	decision := strings.TrimSpace(input.Decision)
	if err := approvals.ResolveApproval(
		request.Context(), session.ConversationID, approvalID, decision,
	); err != nil {
		writeCodeError(response, err)
		return
	}
	approval, err := s.code.ResolveApproval(
		request.Context(), sessionID, authorID, approvalID, decision,
	)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	_, _ = s.code.RecordAction(request.Context(), codework.Action{
		SessionID: sessionID, Kind: "approval_resolved", Status: "complete",
		Summary: decision, ApprovalID: approvalID,
	})
	_, _ = s.code.SetState(
		request.Context(), sessionID, authorID, codework.StateRunning, "",
	)
	writeJSON(response, http.StatusOK, approval)
}

func (s *Server) discardCodeFile(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Path        string `json:"path"`
		DiffVersion string `json:"diff_version"`
	}
	if err := decodeBoundedJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid file discard request"))
		return
	}
	sessionID, authorID := s.codeIdentity(request)
	diff, err := s.code.DiscardFile(
		request.Context(), sessionID, authorID,
		strings.TrimSpace(input.Path), strings.TrimSpace(input.DiffVersion),
	)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, diff)
}

func (s *Server) finishCodeSession(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Message     string `json:"message"`
		DiffVersion string `json:"diff_version"`
	}
	if err := decodeBoundedJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid finish request"))
		return
	}
	sessionID, authorID := s.codeIdentity(request)
	session, err := s.code.Finish(
		request.Context(), sessionID, authorID,
		input.Message, strings.TrimSpace(input.DiffVersion),
	)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, session)
}

func (s *Server) discardCodeSession(response http.ResponseWriter, request *http.Request) {
	sessionID, authorID := s.codeIdentity(request)
	session, err := s.code.Session(request.Context(), sessionID, authorID)
	if err != nil {
		writeCodeError(response, err)
		return
	}
	if err := s.code.Discard(request.Context(), sessionID, authorID); err != nil {
		writeCodeError(response, err)
		return
	}
	if session.ConversationID != "" && s.history != nil {
		if err := s.history.DeleteConversation(request.Context(), session.ConversationID); err != nil {
			slog.Warn("discarded Code worktree but could not delete transcript",
				"session", sessionID, "conversation", session.ConversationID, "error", err)
		}
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) codeIdentity(request *http.Request) (string, string) {
	viewer := s.conversationViewer(request.Context())
	return strings.TrimSpace(request.PathValue("sessionID")), viewer.Author.ID
}

func contextWithoutCancellation(request *http.Request) context.Context {
	return context.WithoutCancel(request.Context())
}

func writeCodeError(response http.ResponseWriter, err error) {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "not found"):
		writeAPIError(response, http.StatusNotFound, err)
	case strings.Contains(message, "not enabled"), strings.Contains(message, "not editable"),
		strings.Contains(message, "active"), strings.Contains(message, "changed"):
		writeAPIError(response, http.StatusConflict, err)
	case strings.Contains(message, "required"), strings.Contains(message, "invalid"),
		strings.Contains(message, "unsafe"), strings.Contains(message, "exceed"):
		writeAPIError(response, http.StatusBadRequest, err)
	case errors.Is(err, agent.ErrConversationForbidden):
		writeAPIError(response, http.StatusForbidden, err)
	default:
		writeAPIError(response, http.StatusUnprocessableEntity, err)
	}
}
