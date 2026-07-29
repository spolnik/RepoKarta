// Package codex adapts the local Codex app-server to RepoKarta conversations.
package codex

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/agent/localcommand"
	"github.com/spolnik/RepoKarta/internal/agent/processgroup"
	"github.com/spolnik/RepoKarta/internal/telemetry"
)

const providerInstructions = `You are RepoKarta's read-only code intelligence assistant.
Answer questions about the indexed repositories using only the RepoKarta MCP tools and image attachments explicitly included in the user's turn.
Ignore personal memory, prior project context, and facts not returned by RepoKarta tools in this session.
Search before drawing conclusions, open the relevant source, and distinguish evidence from inference.
For fleet discovery, request compact search results first and use get_file only for the evidence needed to explain the answer.
Use git_log and git_diff for history questions, then open relevant historical source at the exact returned revision.
Every material code claim must cite the source_url returned by a RepoKarta tool.
Never use shell commands, direct filesystem access beyond supplied image attachments, network search, or code mutation.
If the indexed evidence is insufficient, say so plainly.`

const codingInstructions = `You are RepoKarta's coding agent.
Work only inside the isolated Code worktree supplied as your current directory.
Use RepoKarta's read-only MCP tools for commit-pinned fleet evidence and the worktree filesystem for the change being implemented.
Make the smallest complete change that satisfies the request, preserve unrelated work, and keep every file mutation reviewable through Git.
Do not access paths outside the worktree. Do not fetch, pull, push, publish, open pull requests, or change remotes.
Network access is disabled. Ask for approval before commands when the sandbox requires it.
Explain what changed, what validation ran, and any remaining uncertainty.`

// Adapter starts local Codex app-server sessions.
type Adapter struct {
	Command  string
	statusMu sync.Mutex
	statusAt time.Time
	status   agent.Status
}

func (a *Adapter) ID() string { return "codex" }

func (a *Adapter) Status(ctx context.Context) agent.Status {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if !a.statusAt.IsZero() && time.Since(a.statusAt) < 15*time.Second {
		return a.status
	}
	a.status = a.probeStatus(ctx)
	a.statusAt = time.Now()
	return a.status
}

func (a *Adapter) probeStatus(ctx context.Context) agent.Status {
	status := agent.Status{
		ID:   a.ID(),
		Name: "OpenAI Codex",
		Models: []agent.ModelOption{
			{ID: "gpt-5.6-sol", Label: "gpt-5.6-sol"},
			{ID: "gpt-5.6-terra", Label: "gpt-5.6-terra"},
			{ID: "gpt-5.6-luna", Label: "gpt-5.6-luna"},
		},
		Efforts:      []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"},
		ImageInput:   true,
		ImageOutput:  true,
		Interrupt:    true,
		ContextUsage: true,
		TokenUsage:   true,
		Code:         true,
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
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = "Run `codex login` from the same account and environment that launches RepoKarta"
		}
		status.Detail = "Not authenticated in RepoKarta's launch context. " + detail
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
	command        *exec.Cmd
	processGroup   *processgroup.Group
	stdin          io.WriteCloser
	nextID         atomic.Int64
	writeMu        sync.Mutex
	pendingMu      sync.Mutex
	pending        map[string]chan rpcMessage
	notifications  chan rpcMessage
	serverRequests chan rpcMessage
	readDone       chan struct{}
	readErrMu      sync.Mutex
	readErr        error
	closed         chan struct{}
	threadID       string
	effort         string
	attachments    *agent.AttachmentStore
	activeMu       sync.RWMutex
	activeTurnID   string
	restored       bool
	coding         bool
	workspaceRoot  string
	approvalsMu    sync.Mutex
	approvals      map[string]json.RawMessage
	closeOnce      sync.Once
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
	arguments := codexCommandArguments(config, attachments.Directory())
	process := exec.CommandContext(context.WithoutCancel(ctx), command, arguments...)
	processgroup.Configure(process)
	process.Dir = attachments.Directory()
	if config.Coding {
		process.Dir = config.RepositoryRoot
	}
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
	var stderr processgroup.Buffer
	process.Stderr = &stderr
	if err := process.Start(); err != nil {
		stdin.Close()
		_ = attachments.Close()
		return nil, fmt.Errorf("start Codex app-server: %w", err)
	}
	group, err := processgroup.Attach(process)
	if err != nil {
		_ = process.Process.Kill()
		_ = process.Wait()
		stdin.Close()
		_ = attachments.Close()
		return nil, fmt.Errorf("contain Codex process tree: %w", err)
	}

	s := &session{
		command:        process,
		processGroup:   group,
		stdin:          stdin,
		pending:        make(map[string]chan rpcMessage),
		notifications:  make(chan rpcMessage, 128),
		serverRequests: make(chan rpcMessage, 32),
		readDone:       make(chan struct{}),
		closed:         make(chan struct{}),
		effort:         config.Effort,
		attachments:    attachments,
		coding:         config.Coding,
		workspaceRoot:  config.RepositoryRoot,
		approvals:      make(map[string]json.RawMessage),
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
		"cwd":                   attachments.Directory(),
		"approvalPolicy":        "never",
		"developerInstructions": providerInstructions,
	}
	if config.Coding {
		params["cwd"] = config.RepositoryRoot
		params["approvalPolicy"] = "on-request"
		params["sandbox"] = "workspaceWrite"
		params["developerInstructions"] = codingInstructions
	}
	if config.Model != "" {
		params["model"] = config.Model
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	method := "thread/start"
	if strings.TrimSpace(config.ResumeCursor) != "" {
		method = "thread/resume"
		params["threadId"] = strings.TrimSpace(config.ResumeCursor)
	}
	if err := s.call(initializeContext, method, params, &started); err != nil {
		if method == "thread/resume" {
			delete(params, "threadId")
			if fallbackErr := s.call(initializeContext, "thread/start", params, &started); fallbackErr == nil {
				method = "thread/start"
			} else {
				s.Close()
				return nil, fmt.Errorf(
					"resume Codex thread: %v; fresh start fallback: %w",
					err,
					fallbackErr,
				)
			}
		} else {
			s.Close()
			return nil, fmt.Errorf("start Codex thread: %w", err)
		}
	}
	if started.Thread.ID == "" {
		s.Close()
		return nil, errors.New("Codex app-server returned an empty thread id")
	}
	s.threadID = started.Thread.ID
	s.restored = method == "thread/resume"
	return s, nil
}

func codexCommandArguments(config agent.SessionConfig, attachmentDirectory string) []string {
	arguments := []string{
		"app-server",
		"--stdio",
		"-c", "mcp_servers.repokarta.url=" + config.MCPURL,
		"-c", `mcp_servers.repokarta.bearer_token_env_var="REPOKARTA_MCP_BEARER_TOKEN"`,
		"-c", "tools.web_search=false",
	}
	if config.Coding {
		return append(arguments,
			"-c", `approval_policy="on-request"`,
			"-c", `sandbox_mode="workspace-write"`,
			"-c", `sandbox_workspace_write.network_access=false`,
			"-c", `sandbox_workspace_write.exclude_slash_tmp=true`,
			"-c", `sandbox_workspace_write.exclude_tmpdir_env_var=true`,
		)
	}
	return append(arguments,
		"-c", `default_permissions="repokarta-mcp-only"`,
		"-c", `permissions.repokarta-mcp-only.extends=":read-only"`,
		"-c", `permissions.repokarta-mcp-only.filesystem.":root"="deny"`,
		"-c", `permissions.repokarta-mcp-only.filesystem.":minimal"="read"`,
		"-c", "permissions.repokarta-mcp-only.filesystem."+strconv.Quote(attachmentDirectory)+`="read"`,
	)
}

func (s *session) ResumeCursor() string { return s.threadID }

func (s *session) Restored() bool { return s.restored }

func (s *session) Send(ctx context.Context, turn agent.Turn, emit func(agent.Event) error) (resultErr error) {
	ctx, finish := telemetry.StartOperation(ctx, telemetry.OperationProviderProcess, telemetry.Labels{
		Provider: "codex",
		Kind:     "turn",
	})
	defer func() { finish(resultErr) }()
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
	if err := s.call(ctx, "turn/start", s.turnStartParams(turn, imagePaths), &started); err != nil {
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
		case <-s.readDone:
			return s.readFailure()
		case message := <-s.notifications:
			switch message.Method {
			case "item/agentMessage/delta":
				var delta struct {
					Delta  string `json:"delta"`
					ItemID string `json:"itemId"`
					TurnID string `json:"turnId"`
				}
				if json.Unmarshal(message.Params, &delta) == nil && delta.TurnID == turnID && delta.Delta != "" {
					if err := emit(agent.Event{Type: agent.EventDelta, SegmentID: delta.ItemID, Text: delta.Delta}); err != nil {
						return err
					}
				}
			case "item/started":
				action, ok := codeActionFromItem(message.Params)
				if ok {
					if err := emit(agent.Event{Type: agent.EventCodeAction, CodeAction: &action}); err != nil {
						return err
					}
				}
			case "item/commandExecution/outputDelta":
				var output struct {
					ItemID string `json:"itemId"`
					TurnID string `json:"turnId"`
					Delta  string `json:"delta"`
				}
				if json.Unmarshal(message.Params, &output) == nil &&
					output.TurnID == turnID && output.Delta != "" {
					action := agent.CodeAction{
						ID: output.ItemID, Kind: "commandExecution",
						Status: "running", Output: output.Delta,
					}
					if err := emit(agent.Event{Type: agent.EventCodeAction, CodeAction: &action}); err != nil {
						return err
					}
				}
			case "item/completed":
				var completed struct {
					TurnID string `json:"turnId"`
					Item   struct {
						ID        string `json:"id"`
						Type      string `json:"type"`
						Status    string `json:"status"`
						SavedPath string `json:"savedPath"`
					} `json:"item"`
				}
				if json.Unmarshal(message.Params, &completed) != nil || completed.TurnID != turnID {
					continue
				}
				switch completed.Item.Type {
				case "agentMessage":
					if err := emit(agent.Event{
						Type:      agent.EventActivity,
						Activity:  agent.ActivityThinking,
						SegmentID: completed.Item.ID,
					}); err != nil {
						return err
					}
				case "imageGeneration":
					if completed.Item.Status != "completed" || completed.Item.SavedPath == "" {
						continue
					}
					image, err := agent.ImageFromFile(completed.Item.SavedPath)
					if err != nil {
						return fmt.Errorf("load Codex generated image: %w", err)
					}
					if err := emit(agent.Event{Type: agent.EventImages, Images: []agent.Image{image}}); err != nil {
						return err
					}
				case "commandExecution", "fileChange":
					action := agent.CodeAction{
						ID: completed.Item.ID, Kind: completed.Item.Type,
						Status: completed.Item.Status,
					}
					if err := emit(agent.Event{Type: agent.EventCodeAction, CodeAction: &action}); err != nil {
						return err
					}
				}
			case "thread/tokenUsage/updated":
				usage, ok := contextUsageFromNotification(message.Params, s.threadID, turnID)
				if ok {
					if err := emit(agent.Event{Type: agent.EventContext, Context: &usage}); err != nil {
						return err
					}
				}
				tokenUsage, ok := tokenUsageFromNotification(message.Params, s.threadID, turnID)
				if ok {
					if err := emit(agent.Event{Type: agent.EventUsage, Usage: &tokenUsage}); err != nil {
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
		case request := <-s.serverRequests:
			approval, ok := s.registerApproval(request, turnID)
			if !ok {
				continue
			}
			if err := emit(agent.Event{Type: agent.EventApproval, Approval: &approval}); err != nil {
				return err
			}
		}
	}
}

func tokenUsageFromNotification(raw json.RawMessage, threadID, turnID string) (agent.Usage, bool) {
	var update struct {
		ThreadID   string `json:"threadId"`
		TurnID     string `json:"turnId"`
		TokenUsage struct {
			Last struct {
				InputTokens  int64 `json:"inputTokens"`
				OutputTokens int64 `json:"outputTokens"`
				TotalTokens  int64 `json:"totalTokens"`
			} `json:"last"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(raw, &update) != nil ||
		update.ThreadID != threadID ||
		update.TurnID != turnID {
		return agent.Usage{}, false
	}
	input := max(int64(0), update.TokenUsage.Last.InputTokens)
	output := max(int64(0), update.TokenUsage.Last.OutputTokens)
	total := max(int64(0), update.TokenUsage.Last.TotalTokens)
	if input == 0 && output == 0 {
		return agent.Usage{}, false
	}
	if total == 0 {
		total = input + output
	}
	return agent.Usage{
		InputTokens:  input,
		OutputTokens: output,
		TotalTokens:  total,
	}, true
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

func (s *session) turnStartParams(turn agent.Turn, imagePaths []string) map[string]any {
	input := make([]map[string]any, 0, 1+len(imagePaths))
	message := agent.PromptWithHistory(turn)
	if message != "" {
		input = append(input, map[string]any{
			"type": "text",
			"text": message,
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
		"threadId": s.threadID,
		"input":    input,
	}
	if s.effort != "" {
		params["effort"] = s.effort
	}
	if s.coding {
		params["cwd"] = s.workspaceRoot
		params["approvalPolicy"] = "on-request"
		params["sandboxPolicy"] = map[string]any{
			"type":           "workspaceWrite",
			"writableRoots":  []string{s.workspaceRoot},
			"networkAccess":  false,
			"readOnlyAccess": map[string]any{"type": "restricted", "includePlatformDefaults": true},
		}
	}
	return params
}

// turnStartParams preserves the read-only adapter contract used by focused
// protocol tests; coding sessions use the session-bound variant above.
func turnStartParams(threadID string, turn agent.Turn, imagePaths []string, effort string) map[string]any {
	return (&session{threadID: threadID, effort: effort}).turnStartParams(turn, imagePaths)
}

// ResolveApproval answers one server-initiated app-server request.
func (s *session) ResolveApproval(_ context.Context, id, decision string) error {
	switch decision {
	case "accept", "acceptForSession", "decline", "cancel":
	default:
		return errors.New("invalid Codex approval decision")
	}
	s.approvalsMu.Lock()
	rawID := s.approvals[id]
	if len(rawID) > 0 {
		delete(s.approvals, id)
	}
	s.approvalsMu.Unlock()
	if len(rawID) == 0 {
		return errors.New("Codex approval is no longer pending")
	}
	return s.write(map[string]any{
		"id":     rawID,
		"result": map[string]any{"decision": decision},
	})
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
	case <-s.readDone:
		return s.readFailure()
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
			if s.coding {
				select {
				case s.serverRequests <- message:
				case <-s.closed:
					return
				}
				continue
			}
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
				select {
				case channel <- message:
				case <-s.closed:
					return
				}
			}
			continue
		}
		if message.Method != "" {
			select {
			case s.notifications <- message:
			case <-s.closed:
				return
			}
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	s.readErrMu.Lock()
	s.readErr = err
	s.readErrMu.Unlock()
	close(s.readDone)
}

func (s *session) registerApproval(message rpcMessage, turnID string) (agent.Approval, bool) {
	var params struct {
		ThreadID           string          `json:"threadId"`
		TurnID             string          `json:"turnId"`
		ItemID             string          `json:"itemId"`
		Reason             string          `json:"reason"`
		Command            json.RawMessage `json:"command"`
		CWD                string          `json:"cwd"`
		GrantRoot          string          `json:"grantRoot"`
		AvailableDecisions []string        `json:"availableDecisions"`
	}
	if json.Unmarshal(message.Params, &params) != nil ||
		(params.TurnID != "" && params.TurnID != turnID) {
		return agent.Approval{}, false
	}
	kind := ""
	switch message.Method {
	case "item/commandExecution/requestApproval":
		kind = "command"
	case "item/fileChange/requestApproval":
		kind = "file_change"
	default:
		_ = s.write(map[string]any{
			"id": message.ID, "result": map[string]any{"decision": "decline"},
		})
		return agent.Approval{}, false
	}
	id := "codex-" + hex.EncodeToString(message.ID)
	s.approvalsMu.Lock()
	s.approvals[id] = append(json.RawMessage(nil), message.ID...)
	s.approvalsMu.Unlock()
	decisions := params.AvailableDecisions
	if len(decisions) == 0 {
		decisions = []string{"accept", "decline", "cancel"}
	}
	directory := relativeCodeDirectory(s.workspaceRoot, params.CWD)
	return agent.Approval{
		ID: id, Kind: kind, Reason: params.Reason,
		Command: commandText(params.Command), Directory: directory,
		AvailableDecisions: decisions,
	}, true
}

func codeActionFromItem(raw json.RawMessage) (agent.CodeAction, bool) {
	var event struct {
		TurnID string `json:"turnId"`
		Item   struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Status  string          `json:"status"`
			Command json.RawMessage `json:"command"`
			Changes []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"changes"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return agent.CodeAction{}, false
	}
	if event.Item.Type != "commandExecution" && event.Item.Type != "fileChange" {
		return agent.CodeAction{}, false
	}
	summary := ""
	if event.Item.Type == "fileChange" && len(event.Item.Changes) > 0 {
		summary = fmt.Sprintf("%d file change(s)", len(event.Item.Changes))
	}
	return agent.CodeAction{
		ID: event.Item.ID, Kind: event.Item.Type, Status: event.Item.Status,
		Summary: summary, Command: commandText(event.Item.Command),
	}, true
}

func commandText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var arguments []string
	if json.Unmarshal(raw, &arguments) == nil {
		return strings.Join(arguments, " ")
	}
	if len(raw) > 4096 {
		raw = raw[:4096]
	}
	return strings.TrimSpace(string(raw))
}

func relativeCodeDirectory(root, directory string) string {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(directory) == "" {
		return ""
	}
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return ""
	}
	if relative == "." {
		return "."
	}
	return filepath.ToSlash(relative)
}

func (s *session) readFailure() error {
	s.readErrMu.Lock()
	defer s.readErrMu.Unlock()
	if s.readErr == nil {
		return io.EOF
	}
	return s.readErr
}

func (s *session) Close() error {
	var closeError error
	s.closeOnce.Do(func() {
		close(s.closed)
		_ = s.stdin.Close()
		_ = s.processGroup.Kill()
		closeError = s.command.Wait()
		closeError = errors.Join(closeError, s.attachments.Close())
	})
	return closeError
}
