// Package claude adapts the local Claude Code CLI stream protocol.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/agent/localcommand"
)

const providerInstructions = `You are RepoKarta's read-only code intelligence assistant.
Answer questions about the indexed repositories using only the RepoKarta MCP tools and image attachments explicitly included in the user's turn.
Search before drawing conclusions, open the relevant source, and distinguish evidence from inference.
Every material code claim must cite the source_url returned by a RepoKarta tool.
Never use shell commands, direct filesystem access beyond exact supplied image attachment paths, network search, or code mutation.
If the indexed evidence is insufficient, say so plainly.`

// Adapter starts local Claude Code stream-json sessions.
type Adapter struct {
	Command string
}

func (a *Adapter) ID() string { return "claude" }

func (a *Adapter) Status(ctx context.Context) agent.Status {
	status := agent.Status{
		ID:               a.ID(),
		Name:             "Anthropic Claude",
		Models:           []string{"sonnet", "opus"},
		ModelPlaceholder: "sonnet, opus, or full model ID",
		Efforts:          []string{"low", "medium", "high", "xhigh", "max"},
		ImageInput:       true,
		ImageOutput:      false,
	}
	command, err := localcommand.Resolve(a.Command, "claude")
	if err != nil {
		status.Detail = "Claude Code CLI was not found"
		return status
	}
	status.Available = true
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(bounded, command, "auth", "status").CombinedOutput()
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
			status.Detail = "Run `claude auth login` to connect your account"
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
	arguments := commandArguments(config, string(mcpConfig), attachments.Directory())

	process := exec.CommandContext(context.WithoutCancel(ctx), command, arguments...)
	process.Dir = config.RepositoryRoot
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
	var stderr strings.Builder
	process.Stderr = &stderr
	if err := process.Start(); err != nil {
		stdin.Close()
		_ = attachments.Close()
		return nil, fmt.Errorf("start Claude Code: %w", err)
	}
	s := &session{
		command:     process,
		stdin:       stdin,
		messages:    make(chan json.RawMessage, 128),
		readError:   make(chan error, 1),
		stderr:      &stderr,
		attachments: attachments,
	}
	go s.read(stdout)
	return s, nil
}

func commandArguments(config agent.SessionConfig, mcpConfig, attachmentDirectory string) []string {
	arguments := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
		"--no-session-persistence",
		"--strict-mcp-config",
		"--mcp-config", mcpConfig,
		"--permission-mode", "plan",
		"--disallowed-tools", "Bash", "Edit", "Write", "NotebookEdit", "WebFetch", "WebSearch",
		"--system-prompt", providerInstructions,
	}
	if attachmentDirectory != "" {
		arguments = append(arguments, "--add-dir", attachmentDirectory)
	}
	if config.Model != "" {
		arguments = append(arguments, "--model", config.Model)
	}
	if config.Effort != "" {
		arguments = append(arguments, "--effort", config.Effort)
	}
	return arguments
}

type session struct {
	command     *exec.Cmd
	stdin       io.WriteCloser
	messages    chan json.RawMessage
	readError   chan error
	stderr      *strings.Builder
	attachments *agent.AttachmentStore
	sendMu      sync.Mutex
	closeOnce   sync.Once
}

func (s *session) Send(ctx context.Context, turn agent.Turn, emit func(agent.Event) error) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

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
	if _, err := s.stdin.Write(encoded); err != nil {
		return fmt.Errorf("send Claude message: %w", err)
	}

	emittedText := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-s.readError:
			detail := strings.TrimSpace(s.stderr.String())
			if detail != "" {
				return fmt.Errorf("Claude Code stopped: %w: %s", err, detail)
			}
			return fmt.Errorf("Claude Code stopped: %w", err)
		case raw := <-s.messages:
			var envelope struct {
				Type    string          `json:"type"`
				Subtype string          `json:"subtype"`
				IsError bool            `json:"is_error"`
				Result  string          `json:"result"`
				Error   string          `json:"error"`
				Event   json.RawMessage `json:"event"`
				Message json.RawMessage `json:"message"`
			}
			if json.Unmarshal(raw, &envelope) != nil {
				continue
			}
			switch envelope.Type {
			case "stream_event":
				var event struct {
					Type  string `json:"type"`
					Delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"delta"`
				}
				if json.Unmarshal(envelope.Event, &event) == nil &&
					event.Type == "content_block_delta" &&
					event.Delta.Type == "text_delta" &&
					event.Delta.Text != "" {
					emittedText = true
					if err := emit(agent.Event{Type: agent.EventDelta, Text: event.Delta.Text}); err != nil {
						return err
					}
				}
			case "assistant":
				if emittedText {
					continue
				}
				for _, text := range assistantText(envelope.Message) {
					emittedText = true
					if err := emit(agent.Event{Type: agent.EventDelta, Text: text}); err != nil {
						return err
					}
				}
			case "result":
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
					if err := emit(agent.Event{Type: agent.EventDelta, Text: envelope.Result}); err != nil {
						return err
					}
				}
				return nil
			}
		}
	}
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
		s.messages <- raw
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

func promptWithAttachments(turn agent.Turn, imagePaths []string) string {
	if len(imagePaths) == 0 {
		return turn.Message
	}
	var prompt strings.Builder
	if turn.Message != "" {
		prompt.WriteString(turn.Message)
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
