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
	Role    llm.Role `json:"role"`
	Content string   `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type apiError struct {
	Message string `json:"message"`
}

type chunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Error *apiError `json:"error"`
}

// Stream implements llm.Provider.
func (c *Client) Stream(ctx context.Context, req llm.Request, onDelta func(string) error) (llm.Message, error) {
	msgs := make([]wireMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = wireMessage{Role: m.Role, Content: m.Content}
	}
	body, err := json.Marshal(chatRequest{Model: req.Model, Messages: msgs, Stream: true})
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
		}
	}
	if err := sc.Err(); err != nil {
		return llm.Message{}, err
	}
	return llm.Message{Role: llm.RoleAssistant, Content: text.String()}, nil
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
