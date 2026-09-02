package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// DefaultOpenRouterBaseURL is OpenRouter's OpenAI-compatible root. It serves
// both /chat/completions and /embeddings, so one API key covers both roles.
const DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"

// OpenRouter implements Completer and Embedder against OpenRouter's
// OpenAI-compatible API.
type OpenRouter struct {
	apiKey     string
	baseURL    string
	chatModel  string
	embedModel string
	embedDim   int
	referer    string
	title      string
	http       *http.Client
	maxRetries int
	// embedBatch bounds how many inputs go in one embeddings request. Large
	// batches are cheaper but a single oversized request fails wholesale.
	embedBatch int
}

// OpenRouterOptions configures the client.
type OpenRouterOptions struct {
	APIKey     string
	BaseURL    string // defaults to DefaultOpenRouterBaseURL
	ChatModel  string
	EmbedModel string
	EmbedDim   int
	// Referer and Title populate OpenRouter's optional attribution headers.
	Referer string
	Title   string
	Timeout time.Duration
	// MaxRetries bounds retries on 429 and 5xx. Defaults to 4.
	MaxRetries int
	EmbedBatch int
}

// NewOpenRouter builds a client. It validates configuration eagerly so a
// missing key surfaces at startup rather than midway through an ingest run.
func NewOpenRouter(o OpenRouterOptions) (*OpenRouter, error) {
	if strings.TrimSpace(o.APIKey) == "" {
		return nil, errors.New("llm: OpenRouter API key is empty")
	}
	if o.BaseURL == "" {
		o.BaseURL = DefaultOpenRouterBaseURL
	}
	if o.ChatModel == "" {
		return nil, errors.New("llm: chat model is required")
	}
	if o.EmbedDim < 1 || o.EmbedDim > 4096 {
		return nil, fmt.Errorf("llm: embedding dimension %d outside FalkorDB's supported range 1..4096", o.EmbedDim)
	}
	if o.Timeout <= 0 {
		o.Timeout = 90 * time.Second
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = 4
	}
	if o.EmbedBatch <= 0 {
		o.EmbedBatch = 64
	}
	return &OpenRouter{
		apiKey:     o.APIKey,
		baseURL:    strings.TrimRight(o.BaseURL, "/"),
		chatModel:  o.ChatModel,
		embedModel: o.EmbedModel,
		embedDim:   o.EmbedDim,
		referer:    o.Referer,
		title:      o.Title,
		http:       &http.Client{Timeout: o.Timeout},
		maxRetries: o.MaxRetries,
		embedBatch: o.EmbedBatch,
	}, nil
}

func (c *OpenRouter) Model() string      { return c.chatModel }
func (c *OpenRouter) EmbedModel() string { return c.embedModel }
func (c *OpenRouter) Dimensions() int    { return c.embedDim }

// ---------------------------------------------------------------- completions

type chatRequest struct {
	Model          string      `json:"model"`
	Messages       []Message   `json:"messages"`
	Temperature    float64     `json:"temperature"`
	MaxTokens      int         `json:"max_tokens,omitempty"`
	ResponseFormat *respFormat `json:"response_format,omitempty"`
}

type respFormat struct {
	Type       string          `json:"type"`
	JSONSchema *schemaEnvelope `json:"json_schema,omitempty"`
}

type schemaEnvelope struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Message string `json:"message"`
	Code    any    `json:"code"`
}

func (c *OpenRouter) Complete(ctx context.Context, req CompleteRequest) (*CompleteResponse, error) {
	if len(req.Messages) == 0 {
		return nil, errors.New("llm: Complete called with no messages")
	}
	body := chatRequest{
		Model:       c.chatModel,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	if req.JSONSchema != nil {
		body.ResponseFormat = &respFormat{
			Type: "json_schema",
			JSONSchema: &schemaEnvelope{
				Name:   req.JSONSchema.Name,
				Strict: req.JSONSchema.Strict,
				Schema: req.JSONSchema.Schema,
			},
		}
	}

	var out chatResponse
	if err := c.do(ctx, "/chat/completions", body, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("llm: openrouter: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, errors.New("llm: openrouter returned no choices")
	}
	ch := out.Choices[0]

	// A truncated completion must not be treated as a successful one. During
	// extraction it would silently drop claims from the graph.
	if ch.FinishReason == "length" {
		return nil, fmt.Errorf("%w (model %s)", ErrTruncated, out.Model)
	}
	if ch.FinishReason == "content_filter" {
		return nil, fmt.Errorf("%w (content filter)", ErrRefused)
	}

	return &CompleteResponse{
		Text:             ch.Message.Content,
		PromptTokens:     out.Usage.PromptTokens,
		CompletionTokens: out.Usage.CompletionTokens,
		Model:            out.Model,
		FinishReason:     ch.FinishReason,
	}, nil
}

// CompleteJSON runs a completion constrained by a JSON schema and unmarshals
// the result into v.
func (c *OpenRouter) CompleteJSON(ctx context.Context, req CompleteRequest, v any) (*CompleteResponse, error) {
	if req.JSONSchema == nil {
		return nil, errors.New("llm: CompleteJSON requires a JSONSchema")
	}
	res, err := c.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(res.Text), v); err != nil {
		return res, fmt.Errorf("llm: decoding structured output from %s: %w", res.Model, err)
	}
	return res, nil
}

// ----------------------------------------------------------------- embeddings

type embedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Model string    `json:"model"`
	Error *apiError `json:"error"`
}

// Embed returns one vector per input, in input order. Inputs are batched, and
// order is restored from each item's index rather than assumed, because the API
// does not guarantee response ordering.
func (c *OpenRouter) Embed(ctx context.Context, inputs []string) ([][]float64, error) {
	if c.embedModel == "" {
		return nil, errors.New("llm: no embedding model configured")
	}
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([][]float64, len(inputs))

	for start := 0; start < len(inputs); start += c.embedBatch {
		end := min(start+c.embedBatch, len(inputs))
		batch := inputs[start:end]

		var res embedResponse
		err := c.do(ctx, "/embeddings", embedRequest{
			Model:      c.embedModel,
			Input:      batch,
			Dimensions: c.embedDim,
		}, &res)
		if err != nil {
			return nil, fmt.Errorf("llm: embedding batch %d..%d: %w", start, end, err)
		}
		if res.Error != nil {
			return nil, fmt.Errorf("llm: openrouter embeddings: %s", res.Error.Message)
		}
		if len(res.Data) != len(batch) {
			return nil, fmt.Errorf("llm: embeddings returned %d vectors for %d inputs", len(res.Data), len(batch))
		}
		for _, d := range res.Data {
			if d.Index < 0 || d.Index >= len(batch) {
				return nil, fmt.Errorf("llm: embeddings returned out-of-range index %d", d.Index)
			}
			// A dimension mismatch must fail here rather than at index time,
			// where FalkorDB would reject the vector with a less obvious error.
			if len(d.Embedding) != c.embedDim {
				return nil, fmt.Errorf("llm: model %s returned %d dimensions, expected %d",
					res.Model, len(d.Embedding), c.embedDim)
			}
			out[start+d.Index] = d.Embedding
		}
	}
	return out, nil
}

// EmbedOne is a convenience wrapper for single-input calls.
func (c *OpenRouter) EmbedOne(ctx context.Context, input string) ([]float64, error) {
	vs, err := c.Embed(ctx, []string{input})
	if err != nil {
		return nil, err
	}
	return vs[0], nil
}

// ------------------------------------------------------------------ transport

// do issues a request with retries on 429 and 5xx, honouring Retry-After when
// present and falling back to exponential backoff with jitter.
func (c *OpenRouter) do(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("llm: encoding request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("llm: building request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Content-Type", "application/json")
		if c.referer != "" {
			req.Header.Set("HTTP-Referer", c.referer)
		}
		if c.title != "" {
			req.Header.Set("X-Title", c.title)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("llm: %s: %w", path, err)
			continue
		}

		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("llm: reading response: %w", readErr)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("llm: decoding %s response: %w", path, err)
			}
			return nil

		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("llm: %s: HTTP %d: %s", path, resp.StatusCode, snippet(raw))
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if d, ok := parseRetryAfter(ra); ok {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(d):
					}
				}
			}
			continue

		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			// Never retry an auth failure - it will not resolve itself, and
			// retrying only delays a clear error.
			return fmt.Errorf("llm: %s: HTTP %d - check LLM_API_KEY: %s",
				path, resp.StatusCode, snippet(raw))

		default:
			return fmt.Errorf("llm: %s: HTTP %d: %s", path, resp.StatusCode, snippet(raw))
		}
	}
	return fmt.Errorf("llm: giving up after %d attempts: %w", c.maxRetries+1, lastErr)
}

// backoff returns an exponentially increasing delay with full jitter, capped so
// a long ingest run cannot stall for minutes on one chunk.
func backoff(attempt int) time.Duration {
	base := time.Duration(math.Pow(2, float64(attempt))) * 500 * time.Millisecond
	if base > 30*time.Second {
		base = 30 * time.Second
	}
	return time.Duration(rand.Int64N(int64(base))) + base/2
}

func parseRetryAfter(v string) (time.Duration, bool) {
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err == nil && secs >= 0 && secs <= 120 {
		return time.Duration(secs) * time.Second, true
	}
	return 0, false
}

func snippet(b []byte) string {
	const max = 400
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

var (
	_ Completer = (*OpenRouter)(nil)
	_ Embedder  = (*OpenRouter)(nil)
)
