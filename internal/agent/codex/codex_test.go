package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
)

func TestMain(m *testing.M) {
	if os.Getenv("REPOKARTA_CODEX_TEST_HELPER") == "1" {
		runCodexTestHelper()
		return
	}
	os.Exit(m.Run())
}

func runCodexTestHelper() {
	if len(os.Args) > 1 && os.Args[1] == "login" {
		fmt.Println("Logged in using ChatGPT")
		return
	}
	mode := os.Getenv("REPOKARTA_CODEX_TEST_MODE")
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		if len(request.ID) == 0 {
			continue
		}
		respond := func(result any) {
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": result})
		}
		switch request.Method {
		case "initialize":
			respond(map[string]any{"serverInfo": map[string]string{"name": "test"}})
		case "thread/resume":
			if mode == "resume-fallback" {
				_ = encoder.Encode(map[string]any{
					"id":    request.ID,
					"error": map[string]any{"code": -32000, "message": "expired thread"},
				})
				continue
			}
			respond(map[string]any{"thread": map[string]string{"id": "restored-thread"}})
		case "thread/start":
			threadID := "fresh-thread"
			if mode == "empty-thread" {
				threadID = ""
			}
			respond(map[string]any{"thread": map[string]string{"id": threadID}})
		case "turn/start":
			respond(map[string]any{"turn": map[string]string{"id": "turn-1"}})
			if mode == "hang" {
				continue
			}
			_ = encoder.Encode(map[string]any{
				"method": "item/agentMessage/delta",
				"params": map[string]any{"turnId": "turn-1", "itemId": "message-1", "delta": "hello"},
			})
			_ = encoder.Encode(map[string]any{
				"method": "item/completed",
				"params": map[string]any{
					"turnId": "turn-1",
					"item":   map[string]any{"id": "message-1", "type": "agentMessage", "status": "completed"},
				},
			})
			_ = encoder.Encode(map[string]any{
				"method": "thread/tokenUsage/updated",
				"params": map[string]any{
					"threadId": "fresh-thread",
					"turnId":   "turn-1",
					"tokenUsage": map[string]any{
						"last":               map[string]int64{"inputTokens": 20, "outputTokens": 5, "totalTokens": 25},
						"modelContextWindow": 100,
					},
				},
			})
			status := "completed"
			var rpcFailure any
			if mode == "turn-failed" {
				status = "failed"
				rpcFailure = map[string]any{"code": 1, "message": "provider failed"}
			}
			_ = encoder.Encode(map[string]any{
				"method": "turn/completed",
				"params": map[string]any{
					"turn": map[string]any{"id": "turn-1", "status": status, "error": rpcFailure},
				},
			})
		case "turn/interrupt":
			respond(map[string]any{})
		default:
			respond(map[string]any{})
		}
	}
}

func testCodexAdapter(t *testing.T, mode string) *Adapter {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("REPOKARTA_CODEX_TEST_HELPER", "1")
	t.Setenv("REPOKARTA_CODEX_TEST_MODE", mode)
	return &Adapter{Command: executable}
}

func TestAdapterRunsAuthenticatedStreamingSession(t *testing.T) {
	adapter := testCodexAdapter(t, "")
	status := adapter.Status(context.Background())
	if !status.Available || !status.Authenticated || !status.TokenUsage || status.ID != "codex" {
		t.Fatalf("unexpected status: %#v", status)
	}

	started, err := adapter.Start(context.Background(), agent.SessionConfig{
		Model:    "gpt-5.6-sol",
		Effort:   "high",
		MCPURL:   "http://127.0.0.1:7331/mcp",
		MCPToken: "test-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := started.(*session)
	defer session.Close()
	if session.ResumeCursor() != "fresh-thread" || session.Restored() {
		t.Fatalf("unexpected session identity: cursor=%q restored=%v", session.ResumeCursor(), session.Restored())
	}

	var events []agent.Event
	err = session.Send(context.Background(), agent.Turn{Message: "hello"}, func(event agent.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 ||
		events[0].Type != agent.EventDelta ||
		events[1].Type != agent.EventActivity ||
		events[2].Type != agent.EventContext ||
		events[3].Type != agent.EventUsage {
		t.Fatalf("unexpected events: %#v", events)
	}
	if events[2].Context.UsedTokens != 25 || events[3].Usage.TotalTokens != 25 {
		t.Fatalf("unexpected usage events: %#v", events)
	}

	if err := session.Interrupt(context.Background()); err != agent.ErrNoActiveTurn {
		t.Fatalf("idle interrupt error = %v", err)
	}
	session.setActiveTurn("manual-turn")
	if err := session.Interrupt(context.Background()); err != nil {
		t.Fatalf("active interrupt: %v", err)
	}
	session.clearActiveTurn("another-turn")
	if session.activeTurnID != "manual-turn" {
		t.Fatalf("mismatched clear removed active turn: %q", session.activeTurnID)
	}
	session.clearActiveTurn("manual-turn")
}

func TestCommandArgumentsDenyFilesystemOutsideAttachments(t *testing.T) {
	arguments := codexCommandArguments(agent.SessionConfig{
		MCPURL:   "http://127.0.0.1:7331/mcp",
		MCPToken: "must-not-appear",
	}, `C:\Temp\repokarta-attachments`)
	joined := strings.Join(arguments, "\n")
	for _, expected := range []string{
		`default_permissions="repokarta-mcp-only"`,
		`permissions.repokarta-mcp-only.filesystem.":root"="deny"`,
		`permissions.repokarta-mcp-only.filesystem.":minimal"="read"`,
		`C:\\Temp\\repokarta-attachments`,
		"tools.web_search=false",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("Codex boundary arguments omit %q:\n%s", expected, joined)
		}
	}
	if strings.Contains(joined, "must-not-appear") {
		t.Fatalf("MCP token leaked into Codex argv:\n%s", joined)
	}
}

func TestAdapterFallsBackFromExpiredResumeCursor(t *testing.T) {
	adapter := testCodexAdapter(t, "resume-fallback")
	started, err := adapter.Start(context.Background(), agent.SessionConfig{
		MCPURL:       "http://127.0.0.1:7331/mcp",
		MCPToken:     "test-token",
		ResumeCursor: "expired-thread",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := started.(*session)
	defer session.Close()
	if session.ResumeCursor() != "fresh-thread" || session.Restored() {
		t.Fatalf("fallback session = cursor %q, restored %v", session.ResumeCursor(), session.Restored())
	}
}

func TestAdapterReportsProviderFailureAndCancellation(t *testing.T) {
	t.Run("provider failure", func(t *testing.T) {
		sessionValue, err := testCodexAdapter(t, "turn-failed").Start(context.Background(), agent.SessionConfig{
			MCPURL: "http://127.0.0.1:7331/mcp",
		})
		if err != nil {
			t.Fatal(err)
		}
		session := sessionValue.(*session)
		defer session.Close()
		err = session.Send(context.Background(), agent.Turn{Message: "fail"}, func(agent.Event) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "provider failed") {
			t.Fatalf("failure = %v", err)
		}
	})

	t.Run("cancellation interrupts active turn", func(t *testing.T) {
		sessionValue, err := testCodexAdapter(t, "hang").Start(context.Background(), agent.SessionConfig{
			MCPURL: "http://127.0.0.1:7331/mcp",
		})
		if err != nil {
			t.Fatal(err)
		}
		session := sessionValue.(*session)
		defer session.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		err = session.Send(ctx, agent.Turn{Message: "wait"}, func(agent.Event) error { return nil })
		if err == nil || !errorsIsDeadline(err) {
			t.Fatalf("cancellation = %v", err)
		}
	})
}

func errorsIsDeadline(err error) bool {
	return err == context.DeadlineExceeded || strings.Contains(err.Error(), context.DeadlineExceeded.Error())
}

func TestAdapterRejectsEmptyThreadID(t *testing.T) {
	_, err := testCodexAdapter(t, "empty-thread").Start(context.Background(), agent.SessionConfig{
		MCPURL: "http://127.0.0.1:7331/mcp",
	})
	if err == nil || !strings.Contains(err.Error(), "empty thread id") {
		t.Fatalf("start error = %v", err)
	}
}

func TestTurnStartParamsIncludeEffort(t *testing.T) {
	params := turnStartParams("thread", agent.Turn{Message: "prompt"}, nil, "xhigh")
	if params["effort"] != "xhigh" {
		t.Fatalf("effort = %#v", params["effort"])
	}
}

func TestContextUsageFromNotificationUsesCurrentRequestTokens(t *testing.T) {
	raw := json.RawMessage(`{
		"threadId":"thread",
		"turnId":"turn",
		"tokenUsage":{
			"total":{"totalTokens":350000},
			"last":{"inputTokens":42000,"outputTokens":8000,"totalTokens":50000},
			"modelContextWindow":200000
		}
	}`)
	usage, ok := contextUsageFromNotification(raw, "thread", "turn")
	if !ok {
		t.Fatal("context usage was not parsed")
	}
	if usage.UsedTokens != 50000 || usage.MaxTokens != 200000 || usage.Percentage != 25 {
		t.Fatalf("unexpected context usage: %#v", usage)
	}
}

func TestTokenUsageFromNotificationReportsProviderBreakdown(t *testing.T) {
	raw := json.RawMessage(`{
		"threadId":"thread",
		"turnId":"turn",
		"tokenUsage":{
			"last":{"inputTokens":42000,"cachedInputTokens":12000,"outputTokens":8000,"reasoningOutputTokens":3000,"totalTokens":50000},
			"modelContextWindow":200000
		}
	}`)
	usage, ok := tokenUsageFromNotification(raw, "thread", "turn")
	if !ok {
		t.Fatal("token usage was not parsed")
	}
	if usage.InputTokens != 42000 || usage.OutputTokens != 8000 || usage.TotalTokens != 50000 {
		t.Fatalf("unexpected token usage: %#v", usage)
	}
	if _, ok := tokenUsageFromNotification(raw, "another-thread", "turn"); ok {
		t.Fatal("usage from another thread was accepted")
	}
}

func TestTurnStartParamsLeaveProviderEffortDefault(t *testing.T) {
	params := turnStartParams("thread", agent.Turn{Message: "prompt"}, nil, "")
	if _, exists := params["effort"]; exists {
		t.Fatalf("default params unexpectedly contain effort: %#v", params)
	}
}

func TestTurnStartParamsIncludeLocalImages(t *testing.T) {
	params := turnStartParams(
		"thread",
		agent.Turn{Message: "inspect this"},
		[]string{`C:\temp\one.png`, `C:\temp\two.webp`},
		"high",
	)
	input, ok := params["input"].([]map[string]any)
	if !ok || len(input) != 3 {
		t.Fatalf("input = %#v", params["input"])
	}
	if input[0]["type"] != "text" || input[1]["type"] != "localImage" || input[2]["path"] != `C:\temp\two.webp` {
		t.Fatalf("unexpected input items: %#v", input)
	}
}
