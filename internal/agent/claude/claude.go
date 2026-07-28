// Package claude adapts the local Claude Code CLI stream protocol.
package claude

import (
	"bufio"
	"context"
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
Never use shell commands, direct filesystem access beyond exact supplied image attachment paths, network search, or code mutation.
If the indexed evidence is insufficient, say so plainly.`

const (
	defaultModel  = "claude-opus-5"
	defaultEffort = "medium"
)

// Adapter starts local Claude Code stream-json sessions.
type Adapter struct {
	Command  string
	statusMu sync.Mutex
	statusAt time.Time
	status   agent.Status
}

func (a *Adapter) ID() string { return "claude" }

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
	efforts := []string{"low", "medium", "high", "xhigh", "max"}
	status := agent.Status{
		ID:   a.ID(),
		Name: "Anthropic Claude",
		Models: []agent.ModelOption{
			{ID: "claude-opus-5", Label: "Opus 5", Efforts: efforts},
			{ID: "claude-fable-5", Label: "Fable 5", Efforts: efforts},
			{ID: "claude-opus-4-8", Label: "Opus 4.8", Efforts: efforts},
			{ID: "claude-sonnet-5", Label: "Sonnet 5", Efforts: efforts},
			{ID: "claude-haiku-4-5", Label: "Haiku 4.5", Efforts: []string{}},
		},
		Efforts:      efforts,
		ImageInput:   true,
		ImageOutput:  false,
		Interrupt:    true,
		ContextUsage: true,
	}
	command, err := localcommand.Resolve(a.Command, "claude")
	if err != nil {
		status.Detail = "Claude Code CLI was not found"
		return status
	}
	status.Available = true
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(bounded, command, statusCommandArguments()...).CombinedOutput()
	if err != nil && len(output) == 0 {
		status.Detail = err.Error()
		return status
	}
	var auth struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if json.Unmarshal(output, &auth) == nil {
		status.Authenticated = auth.LoggedIn
		if auth.LoggedIn {
			status.Detail = "Uses your existing Claude login"
			if auth.AuthMethod != "" {
				status.Detail += " (" + auth.AuthMethod + ")"
			}
		} else {
			status.Detail = "Not authenticated in RepoKarta's launch context. Run `claude auth login` from the same account and environment that launches RepoKarta"
		}
		return status
	}
	status.Detail = strings.TrimSpace(string(output))
	return status
}

func (a *Adapter) Start(ctx context.Context, config agent.SessionConfig) (agent.Session, error) {
	command, err := localcommand.Resolve(a.Command, "claude")
	if err != nil {
		return nil, fmt.Errorf("find Claude CLI: %w", err)
	}
	attachments, err := agent.NewAttachmentStore()
	if err != nil {
		return nil, fmt.Errorf("create Claude attachment store: %w", err)
	}
	mcpConfig, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"repokarta": map[string]any{
				"type": "http",
				"url":  config.MCPURL,
				"headers": map[string]string{
					"Authorization": "Bearer " + config.MCPToken,
				},
			},
		},
	})
	if err != nil {
		_ = attachments.Close()
		return nil, err
	}
	mcpConfigPath := filepath.Join(attachments.Directory(), "mcp.json")
	if err := os.WriteFile(mcpConfigPath, mcpConfig, 0o600); err != nil {
		_ = attachments.Close()
		return nil, fmt.Errorf("write Claude MCP configuration: %w", err)
	}
	arguments := commandArguments(config, mcpConfigPath, attachments.Directory())

	process := newCommand(context.WithoutCancel(ctx), command, arguments, attachments.Directory())
	processgroup.Configure(process)
	// Run from the attachment sandbox rather than a repository or user project
	// directory. Only user settings are loaded, so operational env, hooks, and
	// telemetry apply while project/local settings and repository memory do not.
	stdin, err := process.StdinPipe()
	if err != nil {
		_ = attachments.Close()
		return nil, fmt.Errorf("open Claude stdin: %w", err)
	}
	stdout, err := process.StdoutPipe()
	if err != nil {
		stdin.Close()
		_ = attachments.Close()
		return nil, fmt.Errorf("open Claude stdout: %w", err)
	}
	var stderr processgroup.Buffer
	process.Stderr = &stderr
	if err := process.Start(); err != nil {
		stdin.Close()
		_ = attachments.Close()
		return nil, fmt.Errorf("start Claude Code: %w", err)
	}
	group, err := processgroup.Attach(process)
	if err != nil {
		_ = process.Process.Kill()
		_ = process.Wait()
		stdin.Close()
		_ = attachments.Close()
		return nil, fmt.Errorf("contain Claude process tree: %w", err)
	}
	s := &session{
		command:      process,
		processGroup: group,
		stdin:        stdin,
		messages:     make(chan json.RawMessage, 128),
		readDone:     make(chan struct{}),
		closed:       make(chan struct{}),
		stderr:       &stderr,
		attachments:  attachments,
		pending:      make(map[string]chan controlResponse),
		sessionID:    strings.TrimSpace(config.ResumeCursor),
		restored:     strings.TrimSpace(config.ResumeCursor) != "",
	}
	go s.read(stdout)
	return s, nil
}

func commandArguments(config agent.SessionConfig, mcpConfigPath, attachmentDirectory string) []string {
	arguments := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
		"--strict-mcp-config",
		"--mcp-config", mcpConfigPath,
		"--setting-sources", "user",
		"--settings", "{}",
		"--exclude-dynamic-system-prompt-sections",
		"--permission-mode", "plan",
		"--disallowed-tools", "Bash", "Edit", "Write", "NotebookEdit", "Glob", "Grep", "Task", "Agent", "WebFetch", "WebSearch",
		"--system-prompt", providerInstructions,
	}
	if attachmentDirectory != "" {
		arguments = append(arguments, "--add-dir", attachmentDirectory)
		arguments = append(arguments,
			"--allowed-tools",
			"mcp__repokarta__*",
			"Read("+filepath.ToSlash(attachmentDirectory)+"/**)",
		)
	}
	model := strings.TrimSpace(config.Model)
	if model == "" {
		model = defaultModel
	}
	effort := strings.TrimSpace(config.Effort)
	if effort == "" && model == defaultModel {
		effort = defaultEffort
	}
	arguments = append(arguments, "--model", model)
	if effort != "" {
		arguments = append(arguments, "--effort", effort)
	}
	if strings.TrimSpace(config.ResumeCursor) != "" {
		arguments = append(arguments, "--resume", strings.TrimSpace(config.ResumeCursor))
	}
	return arguments
}

func statusCommandArguments() []string {
	return []string{"--setting-sources", "user", "auth", "status"}
}

// newCommand deliberately leaves Env nil. os/exec then inherits the complete
// parent environment, matching a normal Claude invocation from RepoKarta's
// launch context.
func newCommand(ctx context.Context, command string, arguments []string, directory string) *exec.Cmd {
	process := exec.CommandContext(ctx, command, arguments...)
	process.Dir = directory
	return process
}

type session struct {
	command      *exec.Cmd
	processGroup *processgroup.Group
	stdin        io.WriteCloser
	messages     chan json.RawMessage
	readDone     chan struct{}
	readErrMu    sync.Mutex
	readErr      error
	closed       chan struct{}
	stderr       *processgroup.Buffer
	attachments  *agent.AttachmentStore
	writeMu      sync.Mutex
	pendingMu    sync.Mutex
	pending      map[string]chan controlResponse
	nextControl  atomic.Uint64
	active       atomic.Bool
	interrupted  atomic.Bool
	sendMu       sync.Mutex
	cursorMu     sync.RWMutex
	sessionID    string
	restored     bool
	closeOnce    sync.Once
}

func (s *session) Send(ctx context.Context, turn agent.Turn, emit func(agent.Event) error) (resultErr error) {
	ctx, finish := telemetry.StartOperation(ctx, telemetry.OperationProviderProcess, telemetry.Labels{
		Provider: "claude",
		Kind:     "turn",
	})
	defer func() { finish(resultErr) }()
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.active.Store(true)
	defer s.active.Store(false)

	imagePaths, err := s.attachments.Write(turn.Images)
	if err != nil {
		return fmt.Errorf("prepare Claude image attachments: %w", err)
	}
	defer s.attachments.Remove(imagePaths)

	message := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": promptWithAttachments(turn, imagePaths),
		},
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := s.write(encoded); err != nil {
		return fmt.Errorf("send Claude message: %w", err)
	}

	emittedText := false
	textSegments := make(map[int]string)
	segmentSequence := 0
	nextTextSegment := func(index int) string {
		segmentSequence++
		segmentID := fmt.Sprintf("claude-%d", segmentSequence)
		textSegments[index] = segmentID
		return segmentID
	}
	for {
		select {
		case <-ctx.Done():
			interruptContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Interrupt(interruptContext)
			cancel()
			return ctx.Err()
		case <-s.readDone:
			err := s.readFailure()
			detail := strings.TrimSpace(s.stderr.String())
			if detail != "" {
				return fmt.Errorf("Claude Code stopped: %w: %s", err, detail)
			}
			return fmt.Errorf("Claude Code stopped: %w", err)
		case raw := <-s.messages:
			var envelope struct {
				Type      string          `json:"type"`
				Subtype   string          `json:"subtype"`
				IsError   bool            `json:"is_error"`
				Result    string          `json:"result"`
				Error     string          `json:"error"`
				Event     json.RawMessage `json:"event"`
				Message   json.RawMessage `json:"message"`
				SessionID string          `json:"session_id"`
			}
			if json.Unmarshal(raw, &envelope) != nil {
				continue
			}
			if envelope.SessionID != "" {
				s.cursorMu.Lock()
				s.sessionID = envelope.SessionID
				s.cursorMu.Unlock()
			}
			switch envelope.Type {
			case "stream_event":
				var event struct {
					Type         string `json:"type"`
					Index        int    `json:"index"`
					ContentBlock struct {
						Type string `json:"type"`
					} `json:"content_block"`
					Delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"delta"`
				}
				if json.Unmarshal(envelope.Event, &event) != nil {
					continue
				}
				if event.Type == "content_block_start" && event.ContentBlock.Type == "text" {
					nextTextSegment(event.Index)
				} else if event.Type == "content_block_delta" &&
					event.Delta.Type == "text_delta" && event.Delta.Text != "" {
					emittedText = true
					segmentID := textSegments[event.Index]
					if segmentID == "" {
						segmentID = nextTextSegment(event.Index)
					}
					if err := emit(agent.Event{
						Type:      agent.EventDelta,
						SegmentID: segmentID,
						Text:      event.Delta.Text,
					}); err != nil {
						return err
					}
				} else if event.Type == "content_block_stop" {
					if segmentID := textSegments[event.Index]; segmentID != "" {
						delete(textSegments, event.Index)
						if err := emit(agent.Event{
							Type:      agent.EventActivity,
							Activity:  agent.ActivityThinking,
							SegmentID: segmentID,
						}); err != nil {
							return err
						}
					}
				}
			case "assistant":
				if emittedText {
					continue
				}
				for index, text := range assistantText(envelope.Message) {
					emittedText = true
					if err := emit(agent.Event{
						Type:      agent.EventDelta,
						SegmentID: fmt.Sprintf("claude-message-%d", index+1),
						Text:      text,
					}); err != nil {
						return err
					}
				}
			case "result":
				if s.interrupted.Swap(false) {
					return agent.ErrInterrupted
				}
				if envelope.IsError || envelope.Subtype == "error" {
					detail := envelope.Error
					if detail == "" {
						detail = envelope.Result
					}
					if detail == "" {
						detail = "Claude turn failed"
					}
					return errors.New(detail)
				}
				if !emittedText && envelope.Result != "" {
					if err := emit(agent.Event{
						Type:      agent.EventDelta,
						SegmentID: "claude-result",
						Text:      envelope.Result,
					}); err != nil {
						return err
					}
				}
				if err := s.emitContextUsage(emit); err != nil {
					return err
				}
				return nil
			}
		}
	}
}

func (s *session) ResumeCursor() string {
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	return s.sessionID
}

func (s *session) Restored() bool { return s.restored }

type controlResponse struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Error     string          `json:"error"`
	Response  json.RawMessage `json:"response"`
}

func (s *session) write(encoded []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.stdin.Write(encoded)
	return err
}

func (s *session) control(ctx context.Context, request map[string]any) (json.RawMessage, error) {
	requestID := strconv.FormatUint(s.nextControl.Add(1), 10)
	responseChannel := make(chan controlResponse, 1)
	s.pendingMu.Lock()
	s.pending[requestID] = responseChannel
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, requestID)
		s.pendingMu.Unlock()
	}()

	encoded, err := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request":    request,
	})
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if err := s.write(encoded); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.readDone:
		return nil, s.readFailure()
	case response := <-responseChannel:
		if response.Subtype != "success" {
			if response.Error == "" {
				response.Error = "Claude control request failed"
			}
			return nil, errors.New(response.Error)
		}
		return response.Response, nil
	}
}

// Interrupt cancels the active Claude turn while retaining its session.
func (s *session) Interrupt(ctx context.Context) error {
	if !s.active.Load() {
		return agent.ErrNoActiveTurn
	}
	s.interrupted.Store(true)
	if _, err := s.control(ctx, map[string]any{
		"subtype":       "interrupt",
		"cancel_queued": true,
	}); err != nil {
		s.interrupted.CompareAndSwap(true, false)
		return err
	}
	return nil
}

func (s *session) emitContextUsage(emit func(agent.Event) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := s.control(ctx, map[string]any{"subtype": "get_context_usage"})
	if err != nil {
		return nil
	}
	usage, ok := contextUsageFromResponse(raw)
	if !ok {
		return nil
	}
	return emit(agent.Event{Type: agent.EventContext, Context: &usage})
}

func contextUsageFromResponse(raw json.RawMessage) (agent.ContextUsage, bool) {
	var response struct {
		TotalTokens int64   `json:"totalTokens"`
		MaxTokens   int64   `json:"maxTokens"`
		Percentage  float64 `json:"percentage"`
		Model       string  `json:"model"`
	}
	if json.Unmarshal(raw, &response) != nil || response.MaxTokens <= 0 {
		return agent.ContextUsage{}, false
	}
	return agent.ContextUsage{
		UsedTokens: max(int64(0), response.TotalTokens),
		MaxTokens:  response.MaxTokens,
		Percentage: min(100, max(0, response.Percentage)),
		Model:      response.Model,
	}, true
}

func assistantText(raw json.RawMessage) []string {
	var message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return nil
	}
	var values []string
	for _, content := range message.Content {
		if content.Type == "text" && content.Text != "" {
			values = append(values, content.Text)
		}
	}
	return values
}

func (s *session) read(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		raw := append(json.RawMessage(nil), scanner.Bytes()...)
		var envelope struct {
			Type     string          `json:"type"`
			Response controlResponse `json:"response"`
		}
		if json.Unmarshal(raw, &envelope) == nil && envelope.Type == "control_response" {
			s.pendingMu.Lock()
			channel := s.pending[envelope.Response.RequestID]
			s.pendingMu.Unlock()
			if channel != nil {
				select {
				case channel <- envelope.Response:
				case <-s.closed:
					return
				}
			}
			continue
		}
		select {
		case s.messages <- raw:
		case <-s.closed:
			return
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

func promptWithAttachments(turn agent.Turn, imagePaths []string) string {
	message := agent.PromptWithHistory(turn)
	if len(imagePaths) == 0 {
		return message
	}
	var prompt strings.Builder
	if message != "" {
		prompt.WriteString(message)
		prompt.WriteString("\n\n")
	}
	prompt.WriteString("The user attached the following image")
	if len(imagePaths) != 1 {
		prompt.WriteString("s")
	}
	prompt.WriteString(". Inspect each one with the Read tool. These exact attachment paths are the only local files you may read:\n")
	for index, path := range imagePaths {
		name := strings.TrimSpace(turn.Images[index].Name)
		if name == "" {
			name = fmt.Sprintf("image %d", index+1)
		}
		fmt.Fprintf(&prompt, "- %q: %s\n", name, path)
	}
	return prompt.String()
}
