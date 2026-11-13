package fakehost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ChatMessage is one conversation entry handed to a model gateway.
type ChatMessage struct {
	Role string // "user" | "assistant"
	Text string
}

// ChatGateway turns a conversation history into streamed text chunks. This
// stands in for the host-side ModelStreamingGateway (Weaver in production);
// the wire protocol above it does not change.
type ChatGateway interface {
	// StreamTurn calls onText for each streamed chunk and returns the
	// gateway failure, if any. It must observe ctx cancellation.
	StreamTurn(ctx context.Context, history []ChatMessage, onText func(string) error) error
}

// EchoGateway replies with the last user message (no provider involved).
type EchoGateway struct{}

func (EchoGateway) StreamTurn(_ context.Context, history []ChatMessage, onText func(string) error) error {
	reply := "echo"
	for _, m := range history {
		if m.Role == "user" {
			reply = m.Text
		}
	}
	return onText(reply)
}

// AnthropicGateway streams from an Anthropic-compatible /v1/messages
// endpoint (Kimi for Coding serves one at api.kimi.com/coding/). The API key
// travels via env to the host process, never via argv (design doc §5.2).
type AnthropicGateway struct {
	BaseURL string // default https://api.kimi.com/coding/
	APIKey  string
	Model   string // default k3
	HTTP    *http.Client
}

// StreamTurn POSTs a streaming messages request and relays text_delta chunks.
func (g *AnthropicGateway) StreamTurn(ctx context.Context, history []ChatMessage, onText func(string) error) error {
	base := g.BaseURL
	if base == "" {
		base = "https://api.kimi.com/coding/"
	}
	model := g.Model
	if model == "" {
		model = "k3"
	}
	httpClient := g.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	msgs := make([]map[string]string, len(history))
	for i, m := range history {
		msgs[i] = map[string]string{"role": m.Role, "content": m.Text}
	}
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 4096,
		"messages":   msgs,
		"stream":     true,
	})
	if err != nil {
		return err
	}
	url := strings.TrimSuffix(base, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+g.APIKey)
	req.Header.Set("x-api-key", g.APIKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("provider status %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return fmt.Errorf("bad SSE event: %w", err)
		}
		switch ev.Type {
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				if err := onText(ev.Delta.Text); err != nil {
					return err
				}
			}
		case "message_stop":
			return nil
		case "error":
			msg := "unknown provider error"
			if ev.Error != nil {
				msg = ev.Error.Message
			}
			return fmt.Errorf("provider error: %s", msg)
		}
	}
	return sc.Err()
}
// DeepSeekGateway streams from an OpenAI-compatible chat completions endpoint
// (api.deepseek.com by default). The API key travels via env to the host
// process, never via argv (design doc §5.2).
type DeepSeekGateway struct {
	BaseURL string // default https://api.deepseek.com
	APIKey  string
	Model   string // default deepseek-chat
	HTTP    *http.Client
}

// StreamTurn POSTs a streaming completion and relays delta chunks.
func (g *DeepSeekGateway) StreamTurn(ctx context.Context, history []ChatMessage, onText func(string) error) error {
	base := g.BaseURL
	if base == "" {
		base = "https://api.deepseek.com"
	}
	model := g.Model
	if model == "" {
		model = "deepseek-chat"
	}
	httpClient := g.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	msgs := make([]map[string]string, len(history))
	for i, m := range history {
		msgs[i] = map[string]string{"role": m.Role, "content": m.Text}
	}
	payload, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   true,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.APIKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("provider status %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return nil
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("bad SSE chunk: %w", err)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if text := chunk.Choices[0].Delta.Content; text != "" {
			if err := onText(text); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
