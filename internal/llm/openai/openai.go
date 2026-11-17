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
	"sync/atomic"

	"github.com/spiiritual/steel/internal/llm"
)

// DefaultBaseURL is the official OpenAI API endpoint.
const DefaultBaseURL = "https://api.openai.com/v1"

// Client talks to a Chat Completions endpoint.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	// noUsage is set once the server rejects stream_options, so later
	// requests go without it.
	noUsage atomic.Bool
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
	// ToolChoice is "none" to forbid calls to Tools.
	ToolChoice string `json:"tool_choice,omitempty"`
	MaxTokens  int    `json:"max_tokens,omitempty"`
	Stream     bool   `json:"stream"`
	// StreamOptions asks for a final chunk with the request's usage.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// wireUsage is OpenAI's usage, plus DeepSeek's spelling of cached tokens.
type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
}

func (u *wireUsage) usage() *llm.Usage {
	cached := u.PromptCacheHitTokens
	if u.PromptTokensDetails != nil {
		cached = max(cached, u.PromptTokensDetails.CachedTokens)
	}
	return &llm.Usage{Input: u.PromptTokens, Cached: cached, Output: u.CompletionTokens}
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
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *apiError  `json:"error"`
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
				Function: wireFunction{Name: tc.Name, Arguments: tc.Arguments},
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
	w := chatRequest{Model: req.Model, Messages: msgs, Tools: tools, MaxTokens: req.MaxTokens, Stream: true}
	if req.NoToolCalls && len(tools) > 0 {
		w.ToolChoice = "none"
	}
	return w
}

// post sends req, asking for usage unless the server has refused that.
func (c *Client) post(ctx context.Context, req llm.Request) (*http.Response, error) {
	for {
		w := toWire(req)
		withUsage := !c.noUsage.Load()
		if withUsage {
			w.StreamOptions = &streamOptions{IncludeUsage: true}
		}
		body, err := json.Marshal(w)
		if err != nil {
			return nil, err
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if c.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		resp, err := c.http.Do(httpReq)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		// Some servers reject fields they don't know. Usage is a nicety:
		// go on without it rather than fail.
		if withUsage && resp.StatusCode == http.StatusBadRequest && bytes.Contains(raw, []byte("stream_options")) {
			c.noUsage.Store(true)
			continue
		}
		return nil, fmt.Errorf("POST %s: %w", httpReq.URL, statusError(resp.Status, raw))
	}
}

// Stream implements llm.Provider.
func (c *Client) Stream(ctx context.Context, req llm.Request, onDelta func(string) error) (llm.Message, error) {
	resp, err := c.post(ctx, req)
	if err != nil {
		return llm.Message{}, err
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return llm.Message{}, fmt.Errorf("POST %s: unexpected content type %q (want text/event-stream): %s", resp.Request.URL, ct, raw)
	}

	// A complete stream ends with [DONE] or a finish_reason (some servers omit [DONE]).
	var done bool
	var finish string
	var usage *llm.Usage

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
			done = true
			break
		}
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return llm.Message{}, fmt.Errorf("decode stream chunk: %w", err)
		}
		if ch.Error != nil {
			return llm.Message{}, fmt.Errorf("api error: %s", ch.Error.Message)
		}
		if ch.Usage != nil {
			usage = ch.Usage.usage()
		}
		for _, choice := range ch.Choices {
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
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
	if !done && finish == "" {
		return llm.Message{}, fmt.Errorf("stream ended before completion: %w", io.ErrUnexpectedEOF)
	}

	msg := llm.Message{Role: llm.RoleAssistant, Content: text.String(), Usage: usage}
	for i, p := range calls {
		id := p.id
		if id == "" { // some local servers omit ids; tool results must reference one
			id = fmt.Sprintf("call_%d", i)
		}
		args := p.args.String()
		if args == "" {
			args = "{}"
		}
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{ID: id, Name: p.name, Arguments: args})
	}
	if finish == "length" {
		return msg, llm.ErrTruncated
	}
	return msg, nil
}

func statusError(status string, raw []byte) error {
	var e struct {
		Error *apiError `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != nil && e.Error.Message != "" {
		return fmt.Errorf("%s: %s", status, e.Error.Message)
	}
	if msg := strings.TrimSpace(string(raw)); msg != "" {
		return fmt.Errorf("%s: %s", status, msg)
	}
	return errors.New(status)
}
