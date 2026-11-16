package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/spiiritual/steel/internal/llm"
	"github.com/spiiritual/steel/internal/tool"
)

// scripted replays canned replies and records the requests it received.
type scripted struct {
	replies []llm.Message
	errs    []error
	reqs    []llm.Request
}

func (s *scripted) Stream(_ context.Context, req llm.Request, _ func(string) error) (llm.Message, error) {
	s.reqs = append(s.reqs, llm.Request{Model: req.Model, Messages: append([]llm.Message(nil), req.Messages...), Tools: req.Tools})
	i := len(s.reqs) - 1
	if i < len(s.errs) && s.errs[i] != nil {
		return llm.Message{}, s.errs[i]
	}
	return s.replies[i], nil
}

func echoTool() tool.Tool {
	return tool.Tool{
		Name:       "echo",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			return "echoed " + string(args), nil
		},
	}
}

func TestTurnRunsToolsUntilFinalAnswer(t *testing.T) {
	p := &scripted{replies: []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "a", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)},
			{ID: "b", Name: "missing", Arguments: json.RawMessage(`{}`)},
		}},
		{Role: llm.RoleAssistant, Content: "done"},
	}}
	ag := New(Config{Provider: p, Model: "m", Tools: tool.NewRegistry(echoTool()), Out: io.Discard})

	if err := ag.Turn(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 2 {
		t.Fatalf("model called %d times, want 2", len(p.reqs))
	}
	if len(p.reqs[0].Tools) != 1 || p.reqs[0].Tools[0].Name != "echo" {
		t.Errorf("tools advertised = %+v", p.reqs[0].Tools)
	}
	// Second request carries both tool results, in call order, linked by ID.
	got := p.reqs[1].Messages
	if len(got) != 4 {
		t.Fatalf("second request has %d messages: %+v", len(got), got)
	}
	if got[2].Role != llm.RoleTool || got[2].ToolCallID != "a" || got[2].Content != `echoed {"x":1}` {
		t.Errorf("result a = %+v", got[2])
	}
	if got[3].Role != llm.RoleTool || got[3].ToolCallID != "b" || got[3].Content != `error: unknown tool "missing"` {
		t.Errorf("result b = %+v", got[3])
	}
	if n := len(ag.history); n != 5 || ag.history[4].Content != "done" {
		t.Errorf("history = %+v", ag.history)
	}
}

func TestTurnRollsBackOnError(t *testing.T) {
	p := &scripted{
		replies: []llm.Message{{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "a", Name: "echo", Arguments: json.RawMessage(`{}`)}}}, {}},
		errs:    []error{nil, errors.New("boom")},
	}
	ag := New(Config{Provider: p, Model: "m", System: "sys", Tools: tool.NewRegistry(echoTool()), Out: io.Discard})

	if err := ag.Turn(context.Background(), "go"); err == nil {
		t.Fatal("want error")
	}
	if len(ag.history) != 1 || ag.history[0].Role != llm.RoleSystem {
		t.Errorf("history not rolled back: %+v", ag.history)
	}
}
