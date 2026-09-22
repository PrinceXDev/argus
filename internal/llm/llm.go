// Package llm wraps the two model calls ARGUS makes, behind interfaces narrow
// enough that swapping providers cannot change system behaviour.
//
// ARGUS uses a language model for exactly two jobs:
//
//	Complete  extract claims and inference edges from source text, at ingest
//	Embed     produce vectors for chunks, claims and entity names
//
// It is never used to author Cypher, and never to perform the reasoning. The
// reasoning is a shortest-path computation in FalkorDB; the model's role at
// query time is to read a returned proof chain out loud. Keeping that boundary
// visible in the type system is deliberate - there is no ReasonAbout method
// here, and there should never be one.
package llm

import (
	"context"
	"errors"
	"fmt"
)

// Completer produces text or structured JSON from a prompt.
type Completer interface {
	// Complete returns a single completion.
	Complete(ctx context.Context, req CompleteRequest) (*CompleteResponse, error)
	// Model reports the model identifier, for provenance and benchmark records.
	Model() string
}

// Embedder produces vectors.
type Embedder interface {
	// Embed returns one vector per input, in input order.
	Embed(ctx context.Context, inputs []string) ([][]float64, error)
	// Dimensions reports the vector width, which must match the FalkorDB vector
	// index this embedder feeds.
	Dimensions() int
	// Model reports the model identifier.
	Model() string
}

// Message is one turn in a completion request.
type Message struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// CompleteRequest describes a single completion.
type CompleteRequest struct {
	Messages []Message
	// Temperature defaults to 0. Extraction and benchmarking both need
	// determinism, so a non-zero value must be set explicitly.
	Temperature float64
	MaxTokens   int
	// JSONSchema, when non-nil, requests structured output conforming to it.
	// Extraction depends on this: a claim with a malformed confidence is worse
	// than no claim at all, because it silently corrupts path weights.
	JSONSchema *JSONSchema
}

// JSONSchema requests a structured response.
type JSONSchema struct {
	Name   string
	Strict bool
	Schema map[string]any
}

// CompleteResponse carries the completion and the accounting that the benchmark
// harness reports as cost per answer.
type CompleteResponse struct {
	Text             string
	PromptTokens     int
	CompletionTokens int
	Model            string
	FinishReason     string
}

// Usage totals tokens across a run.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	Calls            int
}

func (u *Usage) Add(r *CompleteResponse) {
	if r == nil {
		return
	}
	u.PromptTokens += r.PromptTokens
	u.CompletionTokens += r.CompletionTokens
	u.Calls++
}

func (u Usage) String() string {
	return fmt.Sprintf("%d calls, %d prompt + %d completion tokens",
		u.Calls, u.PromptTokens, u.CompletionTokens)
}

// ErrRefused indicates the provider declined to answer. It is distinct from a
// transport error because it must not be retried.
var ErrRefused = errors.New("llm: provider refused the request")

// ErrTruncated indicates the completion hit the token ceiling. During
// extraction this must fail loudly: a truncated JSON payload silently drops
// claims, which would quietly thin the graph rather than raise an error.
var ErrTruncated = errors.New("llm: completion truncated before finishing")
