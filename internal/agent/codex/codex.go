// Package codex adapts the local Codex app-server to RepoKarta conversations.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/agent/localcommand"
)

const providerInstructions = `You are RepoKarta's read-only code intelligence assistant.
Answer questions about the indexed repositories using only the RepoKarta MCP tools and image attachments explicitly included in the user's turn.
Search before drawing conclusions, open the relevant source, and distinguish evidence from inference.
Every material code claim must cite the source_url returned by a RepoKarta tool.
Never use shell commands, direct filesystem access beyond supplied image attachments, network search, or code mutation.
If the indexed evidence is insufficient, say so plainly.`

// Adapter starts local Codex app-server sessions.
type Adapter struct {
	Command string
}

func (a *Adapter) ID() string { return "codex" }

func (a *Adapter) Status(ctx context.Context) agent.Status {
	status := agent.Status{
		ID:               a.ID(),
		Name:             "OpenAI Codex",
		ModelPlaceholder: "Codex model ID or provider default",
		Efforts:          []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"},
		ImageInput:       true,
		ImageOutput:      true,
		Interrupt:        true,
		ContextUsage:     true,
	}
	command, err := localcommand.Resolve(a.Command, "codex")
	if err != nil {
		status.Detail = "Codex CLI was not found"
		return status
	}
	status.Available = true
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(bounded, command, "login", "status").CombinedOutput()
	if err != nil {
		status.Detail = strings.TrimSpace(string(output))
		if status.Detail == "" {
			status.Detail = err.Error()
		}
		return status
	}
	status.Authenticated = strings.Contains(strings.ToLower(string(output)), "logged in")
	if status.Authenticated {
		status.Detail = "Uses your existing ChatGPT/Codex login"
	} else {
		status.Detail = strings.TrimSpace(string(output))
	}
	return status
}

func (a *Adapter) Start(ctx context.Context, config agent.SessionConfig) (agent.Session, error) {
	command, err := localcommand.Resolve(a.Command, "codex")
	if err != nil {
		return nil, fmt.Errorf("find Codex CLI: %w", err)
	}
	session, err := startSession(ctx, command, config)
	if err != nil {
		return nil, err
	}
	return session, nil
}

type session struct {
	command       *exec.Cmd
	stdin         io.WriteCloser
	nextID        atomic.Int64
	writeMu       sync.Mutex
	pendingMu     sync.Mutex
	pending       map[string]chan rpcMessage
	notifications chan rpcMessage
	readError     chan error
	threadID      string
	effort        string
	attachments   *agent.AttachmentStore
	activeMu      sync.RWMutex
	activeTurnID  string
	closeOnce     sync.Once
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func startSession(ctx context.Context, command string, config agent.SessionConfig) (*session, error) {
	attachments, err := agent.NewAttachmentStore()
	if err != nil {
		return nil, fmt.Errorf("create Codex attachment store: %w", err)
	}
	arguments := []string{
		"app-server",
		"--stdio",
		"-c", "mcp_servers.repokarta.url=" + config.MCPURL,
		"-c", `mcp_servers.repokarta.bearer_token_env_var="REPOKARTA_MCP_BEARER_TOKEN"`,
	}
	process := exec.CommandContext(context.WithoutCancel(ctx), command, arguments...)
	process.Dir = config.RepositoryRoot
	process.Env = append(os.Environ(), "REPOKARTA_MCP_BEARER_TOKEN="+config.MCPToken)
	stdin, err := process.StdinPipe()
	if err != nil {
		_ = attachments.Close()
		return nil, fmt.Errorf("open Codex stdin: %w", err)
	}
	stdout, err := process.StdoutPipe()
	if err != nil {
		stdin.Close()
		_ = attachments.Close()
		return nil, fmt.Errorf("open Codex stdout: %w", err)
	}
	var stderr strings.Builder
	process.Stderr = &stderr
	if err := process.Start(); err != nil {
		stdin.Close()
		_ = attachments.Close()
		return nil, fmt.Errorf("start Codex app-server: %w", err)
	}

	s := &session{
		command:       process,
		stdin:         stdin,
		pending:       make(map[string]chan rpcMessage),
		notifications: make(chan rpcMessage, 128),
		readError:     make(chan error, 1),
		effort:        config.Effort,
		attachments:   attachments,
	}
	go s.read(stdout)

	initializeContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var initializeResult any
	if err := s.call(initializeContext, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name":    "repokarta",
			"title":   "RepoKarta",
			"version": "0.1",
		},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, &initializeResult); err != nil {
		s.Close()
		return nil, fmt.Errorf("initialize Codex app-server: %w; %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := s.notify("initialized", map[string]any{}); err != nil {
		s.Close()
		return nil, err
	}

	params := map[string]any{
		"cwd":                   config.RepositoryRoot,
		"approvalPolicy":        "never",
		"sandbox":               "read-only",
		"developerInstructions": providerInstructions,
		"ephemeral":             true,
	}
	if config.Model != "" {
		params["model"] = config.Model
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := s.call(initializeContext, "thread/start", params, &started); err != nil {
		s.Close()
		return nil, fmt.Errorf("start Codex thread: %w", err)
	}
	if started.Thread.ID == "" {
		s.Close()
		return nil, errors.New("Codex app-server returned an empty thread id")
	}
	s.threadID = started.Thread.ID
	return s, nil
}

func (s *session) Send(ctx context.Context, turn agent.Turn, emit func(agent.Event) error) error {
	imagePaths, err := s.attachments.Write(turn.Images)
	if err != nil {
		return fmt.Errorf("prepare Codex image attachments: %w", err)
	}
	defer s.attachments.Remove(imagePaths)

	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := s.call(ctx, "turn/start", turnStartParams(s.threadID, turn, imagePaths, s.effort), &started); err != nil {
		return fmt.Errorf("start Codex turn: %w", err)
	}
	turnID := started.Turn.ID
	if turnID == "" {
		return errors.New("Codex app-server returned an empty turn id")
	}
	s.setActiveTurn(turnID)
	defer s.clearActiveTurn(turnID)
	for {
		select {
		case <-ctx.Done():
			interruptContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Interrupt(interruptContext)
			cancel()
			return ctx.Err()
		case err := <-s.readError:
			return err
		case message := <-s.notifications:
			switch message.Method {
			case "item/agentMessage/delta":
				var delta struct {
					Delta  string `json:"delta"`
					TurnID string `json:"turnId"`
				}
				if json.Unmarshal(message.Params, &delta) == nil && (delta.TurnID == "" || delta.TurnID == turnID) && delta.Delta != "" {
					if err := emit(agent.Event{Type: agent.EventDelta, Text: delta.Delta}); err != nil {
						return err
					}
				}
			case "item/completed":
				var completed struct {
					TurnID string `json:"turnId"`
					Item   struct {
						Type      string `json:"type"`
						Status    string `json:"status"`
						SavedPath string `json:"savedPath"`
					} `json:"item"`
				}
				if json.Unmarshal(message.Params, &completed) != nil ||
					completed.TurnID != turnID ||
					completed.Item.Type != "imageGeneration" ||
					completed.Item.Status != "completed" ||
					completed.Item.SavedPath == "" {
					continue
				}
				image, err := agent.ImageFromFile(completed.Item.SavedPath)
				if err != nil {
					return fmt.Errorf("load Codex generated image: %w", err)
				}
				if err := emit(agent.Event{Type: agent.EventImages, Images: []agent.Image{image}}); err != nil {
					return err
				}
			case "thread/tokenUsage/updated":
				usage, ok := contextUsageFromNotification(message.Params, s.threadID, turnID)
				if ok {
					if err := emit(agent.Event{Type: agent.EventContext, Context: &usage}); err != nil {
						return err
					}
				}
			case "turn/completed":
				var completed struct {
					Turn struct {
						ID     string    `json:"id"`
						Status string    `json:"status"`
						Error  *rpcError `json:"error"`
					} `json:"turn"`
				}
				if json.Unmarshal(message.Params, &completed) != nil || completed.Turn.ID != turnID {
					continue
				}
				if completed.Turn.Status == "failed" {
					if completed.Turn.Error != nil {
						return errors.New(completed.Turn.Error.Message)
					}
					return errors.New("Codex turn failed")
				}
				if completed.Turn.Status == "interrupted" {
					return agent.ErrInterrupted
				}
				return nil
			}
		}
	}
}

func contextUsageFromNotification(raw json.RawMessage, threadID, turnID string) (agent.ContextUsage, bool) {
	var update struct {
		ThreadID   string `json:"threadId"`
		TurnID     string `json:"turnId"`
		TokenUsage struct {
			Last struct {
				TotalTokens int64 `json:"totalTokens"`
			} `json:"last"`
			ModelContextWindow *int64 `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(raw, &update) != nil ||
		update.ThreadID != threadID ||
		update.TurnID != turnID ||
		update.TokenUsage.ModelContextWindow == nil ||
		*update.TokenUsage.ModelContextWindow <= 0 {
		return agent.ContextUsage{}, false
	}
	used := max(int64(0), update.TokenUsage.Last.TotalTokens)
	limit := *update.TokenUsage.ModelContextWindow
	return agent.ContextUsage{
		UsedTokens: used,
		MaxTokens:  limit,
		Percentage: min(100, float64(used)*100/float64(limit)),
	}, true
}

// Interrupt cancels the active Codex turn without discarding the thread.
func (s *session) Interrupt(ctx context.Context) error {
	s.activeMu.RLock()
	turnID := s.activeTurnID
	s.activeMu.RUnlock()
	if turnID == "" {
		return agent.ErrNoActiveTurn
	}
	return s.call(ctx, "turn/interrupt", map[string]any{
		"threadId": s.threadID,
		"turnId":   turnID,
	}, nil)
}

func (s *session) setActiveTurn(turnID string) {
	s.activeMu.Lock()
	s.activeTurnID = turnID
	s.activeMu.Unlock()
}

func (s *session) clearActiveTurn(turnID string) {
	s.activeMu.Lock()
	if s.activeTurnID == turnID {
		s.activeTurnID = ""
	}
	s.activeMu.Unlock()
}

func turnStartParams(threadID string, turn agent.Turn, imagePaths []string, effort string) map[string]any {
	input := make([]map[string]any, 0, 1+len(imagePaths))
	if turn.Message != "" {
		input = append(input, map[string]any{
			"type": "text",
			"text": turn.Message,
		})
	}
	for _, path := range imagePaths {
		input = append(input, map[string]any{
			"type":   "localImage",
			"path":   path,
			"detail": "auto",
		})
	}
	params := map[string]any{
		"threadId": threadID,
		"input":    input,
	}
	if effort != "" {
		params["effort"] = effort
	}
	return params
}

func (s *session) call(ctx context.Context, method string, params any, result any) error {
	id := s.nextID.Add(1)
	idText := strconv.FormatInt(id, 10)
	responseChannel := make(chan rpcMessage, 1)
	s.pendingMu.Lock()
	s.pending[idText] = responseChannel
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, idText)
		s.pendingMu.Unlock()
	}()

	if err := s.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-s.readError:
		return err
	case response := <-responseChannel:
		if response.Error != nil {
			return fmt.Errorf("Codex RPC %s (%d): %s", method, response.Error.Code, response.Error.Message)
		}
		if result != nil && len(response.Result) > 0 {
			if err := json.Unmarshal(response.Result, result); err != nil {
				return fmt.Errorf("decode Codex RPC %s: %w", method, err)
			}
		}
		return nil
	}
}

func (s *session) notify(method string, params any) error {
	return s.write(map[string]any{"method": method, "params": params})
}

func (s *session) write(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	encoded = append(encoded, '\n')
	_, err = s.stdin.Write(encoded)
	return err
}

func (s *session) read(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		var message rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		if len(message.ID) > 0 && message.Method != "" {
			_ = s.write(map[string]any{
				"id": message.ID,
				"error": map[string]any{
					"code":    -32601,
					"message": "RepoKarta runs providers read-only and does not handle approval requests",
				},
			})
			continue
		}
		if len(message.ID) > 0 {
			id := strings.Trim(string(message.ID), `"`)
			s.pendingMu.Lock()
			channel := s.pending[id]
			s.pendingMu.Unlock()
			if channel != nil {
				channel <- message
			}
			continue
		}
		if message.Method != "" {
			s.notifications <- message
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	select {
	case s.readError <- err:
	default:
	}
}

func (s *session) Close() error {
	var closeError error
	s.closeOnce.Do(func() {
		_ = s.stdin.Close()
		if s.command.Process != nil {
			_ = s.command.Process.Kill()
		}
		closeError = s.command.Wait()
		closeError = errors.Join(closeError, s.attachments.Close())
	})
	return closeError
}
