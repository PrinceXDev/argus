// Package config loads ARGUS configuration from the environment.
//
// Configuration is resolved once at startup and passed explicitly. Nothing in
// ARGUS reads os.Getenv outside this package.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	// Graph
	FalkorURL string
	GraphName string
	QueryUser string // optional: read-only ACL user for the API process
	QueryPass string

	// LLM
	LLMProvider string
	LLMModel    string
	LLMAPIKey   string

	// Embeddings
	EmbedProvider string
	EmbedModel    string
	EmbedDim      int
	EmbedAPIKey   string

	// Ingestion
	EdgarUserAgent string

	// Server
	Port     string
	LogLevel string
	DemoMode string // live | replay
}

// Load reads configuration from the environment. Missing optional values fall
// back to defaults; missing required values produce an aggregated error so the
// operator sees every problem at once rather than one per restart.
func Load() (*Config, error) {
	c := &Config{
		FalkorURL:      env("FALKORDB_URL", "falkor://localhost:6379"),
		GraphName:      env("FALKORDB_GRAPH", "argus"),
		QueryUser:      env("FALKORDB_QUERY_USER", ""),
		QueryPass:      env("FALKORDB_QUERY_PASS", ""),
		LLMProvider:    env("LLM_PROVIDER", "openrouter"),
		LLMModel:       env("LLM_MODEL", "openai/gpt-4o-mini"),
		LLMAPIKey:      env("LLM_API_KEY", ""),
		EmbedProvider:  env("EMBED_PROVIDER", "openrouter"),
		EmbedModel:     env("EMBED_MODEL", "openai/text-embedding-3-small"),
		EmbedAPIKey:    env("EMBED_API_KEY", ""),
		EdgarUserAgent: env("EDGAR_USER_AGENT", ""),
		Port:           env("PORT", "8080"),
		LogLevel:       env("LOG_LEVEL", "info"),
		DemoMode:       env("DEMO_MODE", "live"),
	}

	dim, err := strconv.Atoi(env("EMBED_DIM", "1536"))
	if err != nil {
		return nil, fmt.Errorf("EMBED_DIM: %w", err)
	}
	c.EmbedDim = dim

	// OpenRouter serves chat completions and embeddings from the same key, so a
	// blank EMBED_API_KEY falls back to the LLM key rather than failing. Keeping
	// them as separate fields still allows a split setup (for example Anthropic
	// for extraction, which has no embeddings endpoint, plus OpenAI for vectors).
	if c.EmbedAPIKey == "" && c.EmbedProvider == c.LLMProvider {
		c.EmbedAPIKey = c.LLMAPIKey
	}

	var problems []string
	if c.LLMAPIKey == "" {
		problems = append(problems, "LLM_API_KEY is required")
	}
	if c.EmbedAPIKey == "" {
		problems = append(problems, fmt.Sprintf(
			"EMBED_API_KEY is required when EMBED_PROVIDER (%s) differs from LLM_PROVIDER (%s)",
			c.EmbedProvider, c.LLMProvider))
	}
	// FalkorDB's vector index accepts 1..4096 dimensions.
	if c.EmbedDim < 1 || c.EmbedDim > 4096 {
		problems = append(problems, "EMBED_DIM must be between 1 and 4096 (FalkorDB vector index limit)")
	}
	if len(problems) > 0 {
		return nil, errors.New("config: " + strings.Join(problems, "; "))
	}
	return c, nil
}

// LoadIngest validates the subset needed by the ingester, which additionally
// requires an EDGAR contact string but may run without a serving port.
func LoadIngest() (*Config, error) {
	c, err := Load()
	if err != nil {
		return nil, err
	}
	if c.EdgarUserAgent == "" {
		return nil, errors.New("config: EDGAR_USER_AGENT is required for ingestion " +
			"(the SEC rejects requests without a contact email)")
	}
	return c, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// ForSchemaOnly returns a config sufficient to create indexes and constraints.
//
// Schema creation needs a database but no model, so a missing API key must not
// block it. This keeps `argus-ingest -schema-only` usable as a first step
// before any provider credentials exist.
func ForSchemaOnly() *Config {
	dim, err := strconv.Atoi(env("EMBED_DIM", "1536"))
	if err != nil || dim < 1 || dim > 4096 {
		dim = 1536
	}
	return &Config{
		FalkorURL: env("FALKORDB_URL", "falkor://localhost:6379"),
		GraphName: env("FALKORDB_GRAPH", "argus"),
		EmbedDim:  dim,
		LogLevel:  env("LOG_LEVEL", "info"),
	}
}

// QueryURL returns the connection URL the read-only API process should use.
//
// When a dedicated read-only ACL user is configured, its credentials are
// substituted into the URL so the database itself refuses writes from the
// serving process. Without one, the primary URL is returned and the read-only
// guarantee rests on the client alone - which is weaker, and is reported as
// such by argus-doctor.
func (c *Config) QueryURL() string {
	if c.QueryUser == "" || c.QueryPass == "" {
		return c.FalkorURL
	}
	i := strings.Index(c.FalkorURL, "://")
	if i < 0 {
		return c.FalkorURL
	}
	scheme, rest := c.FalkorURL[:i+3], c.FalkorURL[i+3:]
	// Drop any credentials already present before adding our own.
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return scheme + c.QueryUser + ":" + c.QueryPass + "@" + rest
}
