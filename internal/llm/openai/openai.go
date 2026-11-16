// Package openai implements llm.Provider for OpenAI and OpenAI-compatible
// Chat Completions APIs (OpenRouter, Ollama, LM Studio, vLLM, Groq, ...).
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spiiritual/steel/internal/llm"
)

// DefaultBaseURL is the official OpenAI API endpoint.
const DefaultBaseURL = "https://api.openai.com/v1"

// Client talks to a Chat Completions endpoint.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New returns a client for baseURL (e.g. "http://localhost:11434/v1").
// apiKey may be empty for local servers that don't require auth.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		// No client timeout: streams can be long; cancellation comes from ctx.
		http: &http.Client{},
	}
}

type wireMessage struct {
	Role llm.Role `json:"role"`
	// Content is null on assistant messages that only carry tool calls.
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string      `json:"type"`
	Function wireToolDef `json:"function"`
}

type wireToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
}

type apiError struct {
	Message string `json:"message"`
}

type chunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Error *apiError `json:"error"`
}

func toWire(req llm.Request) chatRequest {
	msgs := make([]wireMessage, len(req.Messages))
	for i, m := range req.Messages {
		w := wireMessage{Role: m.Role, ToolCallID: m.ToolCallID}
		if m.Content != "" || len(m.ToolCalls) == 0 {
			w.Content = &m.Content
		}
		for _, tc := range m.ToolCalls {
			w.ToolCalls = append(w.ToolCalls, wireToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: wireFunction{Name: tc.Name, Arguments: string(tc.Arguments)},
			})
		}
		msgs[i] = w
	}
	var tools []wireTool
	for _, t := range req.Tools {
		tools = append(tools, wireTool{
			Type:     "function",
			Function: wireToolDef{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	return chatRequest{Model: req.Model, Messages: msgs, Tools: tools, Stream: true}
}

// Stream implements llm.Provider.
func (c *Client) Stream(ctx context.Context, req llm.Request, onDelta func(string) error) (llm.Message, error) {
	body, err := json.Marshal(toWire(req))
	if err != nil {
		return llm.Message{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return llm.Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return llm.Message{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return llm.Message{}, fmt.Errorf("POST %s: %w", httpReq.URL, statusError(resp))
	}

	var text strings.Builder
	// Tool call fragments arrive keyed by index; arguments are split across chunks.
	type partialCall struct {
		id, name string
		args     strings.Builder
	}
	var calls []*partialCall

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue // blank separators, comments, event:/id: fields
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return llm.Message{}, fmt.Errorf("decode stream chunk: %w", err)
		}
		if ch.Error != nil {
			return llm.Message{}, fmt.Errorf("api error: %s", ch.Error.Message)
		}
		for _, choice := range ch.Choices {
			if d := choice.Delta.Content; d != "" {
				text.WriteString(d)
				if err := onDelta(d); err != nil {
					return llm.Message{}, err
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				if tc.Index < 0 || tc.Index > len(calls) {
					return llm.Message{}, fmt.Errorf("tool call index %d out of order", tc.Index)
				}
				if tc.Index == len(calls) {
					calls = append(calls, &partialCall{})
				}
				p := calls[tc.Index]
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name = tc.Function.Name
				}
				p.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return llm.Message{}, err
	}

	msg := llm.Message{Role: llm.RoleAssistant, Content: text.String()}
	for i, p := range calls {
		id := p.id
		if id == "" { // some local servers omit ids; tool results must reference one
			id = fmt.Sprintf("call_%d", i)
		}
		args := p.args.String()
		if args == "" {
			args = "{}"
		}
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{ID: id, Name: p.name, Arguments: json.RawMessage(args)})
	}
	return msg, nil
}

func statusError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error *apiError `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != nil && e.Error.Message != "" {
		return fmt.Errorf("%s: %s", resp.Status, e.Error.Message)
	}
	if msg := strings.TrimSpace(string(raw)); msg != "" {
		return fmt.Errorf("%s: %s", resp.Status, msg)
	}
	return errors.New(resp.Status)
}
