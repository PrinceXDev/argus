// Package kg is the only package in ARGUS that talks to FalkorDB.
//
// It enforces two invariants that the rest of the system depends on:
//
//  1. The read path and the write path are different Go types. A package that
//     is handed a Reader cannot write, regardless of what Cypher it composes.
//     This mirrors the database-level ACL split (%R~argus* vs %RW~argus) so the
//     boundary holds even if one of the two layers is misconfigured.
//
//  2. No caller outside this package composes Cypher from untrusted input.
//     Queries are named templates with typed parameters (see templates.go).
package kg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	falkordb "github.com/FalkorDB/falkordb-go/v2"
)

// Reader is the read-only surface. Every query issued through it uses
// GRAPH.RO_QUERY, so a write clause is rejected by the server even if one is
// somehow constructed.
type Reader interface {
	// Read executes a named template against the primary graph.
	Read(ctx context.Context, t Template, params Params) (Rows, error)
	// ReadGraph executes a named template against a specific graph key. Used by
	// the retraction engine to query a fork.
	ReadGraph(ctx context.Context, graph string, t Template, params Params) (Rows, error)
	// Plan returns the execution plan for a template without running it.
	Plan(ctx context.Context, t Template, params Params) (string, error)
	// GraphName is the primary graph key.
	GraphName() string
	// Close releases the connection.
	Close() error
}

// Writer adds the mutating surface. Only the ingester and the retraction
// engine's fork manager ever receive one.
type Writer interface {
	Reader
	// Write executes a named mutating template against the primary graph.
	Write(ctx context.Context, t Template, params Params) (Rows, error)
	// WriteGraph executes a named mutating template against a specific graph key.
	WriteGraph(ctx context.Context, graph string, t Template, params Params) (Rows, error)
	// Raw issues a bare Cypher string against a specific graph. It exists only
	// for schema DDL (constraints and indexes), which is not expressible as a
	// parameterised template. It is never reachable from a request path.
	Raw(ctx context.Context, graph, cypher string) (Rows, error)
	// CopyGraph duplicates src to dst. falkordb-go v2.1.0 does not wrap
	// GRAPH.COPY, so this is the single raw-command escape hatch in ARGUS.
	// The source graph stays fully readable for the duration of the copy.
	CopyGraph(ctx context.Context, src, dst string) error
	// DropGraph deletes a graph key. Guarded: it refuses to delete the primary
	// graph, so a fork-reaper bug cannot destroy the corpus.
	DropGraph(ctx context.Context, graph string) error
	// ListGraphs returns every graph key on the instance.
	ListGraphs(ctx context.Context) ([]string, error)
}

// Params carries typed query parameters. Values must be scalars, []interface{},
// or map[string]interface{} - the types falkordb-go serialises.
type Params map[string]interface{}

// Client is the concrete FalkorDB-backed implementation of Reader and Writer.
type Client struct {
	db        *falkordb.FalkorDB
	graphName string
	timeout   time.Duration
	readOnly  bool
}

// Options configures a Client.
type Options struct {
	URL       string
	GraphName string
	// Timeout bounds a single query. FalkorDB enforces this server-side via
	// QueryOptions, so a runaway traversal cannot pin a thread indefinitely.
	Timeout time.Duration
	// ReadOnly makes Write calls fail closed at the client, in addition to the
	// server-side ACL. Defence in depth: a misconfigured ACL is then still safe.
	ReadOnly bool
}

// Dial connects to FalkorDB. The URL accepts falkor:// and falkors:// (TLS),
// with optional user:password credentials - the form FalkorDB Cloud issues.
func Dial(opts Options) (*Client, error) {
	if opts.GraphName == "" {
		return nil, fmt.Errorf("kg: GraphName is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	db, err := falkordb.FromURL(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("kg: connect %s: %w", redactURL(opts.URL), err)
	}
	return &Client{
		db:        db,
		graphName: opts.GraphName,
		timeout:   opts.Timeout,
		readOnly:  opts.ReadOnly,
	}, nil
}

func (c *Client) GraphName() string { return c.graphName }

func (c *Client) Close() error {
	if c.db == nil || c.db.Conn == nil {
		return nil
	}
	return c.db.Conn.Close()
}

// Ping verifies the connection. Used by the health endpoint and by doctor.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.db.Conn.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("kg: ping: %w", err)
	}
	return nil
}

func (c *Client) Read(ctx context.Context, t Template, params Params) (Rows, error) {
	return c.ReadGraph(ctx, c.graphName, t, params)
}

func (c *Client) ReadGraph(ctx context.Context, graph string, t Template, params Params) (Rows, error) {
	if t.Mode != ModeRead {
		return nil, fmt.Errorf("kg: template %q is a write template and cannot be issued on the read path", t.Name)
	}
	cypher, err := t.render(params)
	if err != nil {
		return nil, err
	}
	res, err := c.db.SelectGraph(graph).ROQuery(cypher, params, c.queryOpts())
	if err != nil {
		return nil, fmt.Errorf("kg: read %s on %s: %w", t.Name, graph, err)
	}
	return rowsFrom(res)
}

func (c *Client) Plan(ctx context.Context, t Template, params Params) (string, error) {
	cypher, err := t.render(params)
	if err != nil {
		return "", err
	}
	plan, err := c.db.SelectGraph(c.graphName).ExecutionPlan(cypher)
	if err != nil {
		return "", fmt.Errorf("kg: plan %s: %w", t.Name, err)
	}
	return plan, nil
}

func (c *Client) Write(ctx context.Context, t Template, params Params) (Rows, error) {
	return c.WriteGraph(ctx, c.graphName, t, params)
}

func (c *Client) WriteGraph(ctx context.Context, graph string, t Template, params Params) (Rows, error) {
	if c.readOnly {
		return nil, fmt.Errorf("kg: write %s refused: client is read-only", t.Name)
	}
	cypher, err := t.render(params)
	if err != nil {
		return nil, err
	}
	res, err := c.db.SelectGraph(graph).Query(cypher, params, c.queryOpts())
	if err != nil {
		return nil, fmt.Errorf("kg: write %s on %s: %w", t.Name, graph, err)
	}
	return rowsFrom(res)
}

func (c *Client) Raw(ctx context.Context, graph, cypher string) (Rows, error) {
	if c.readOnly {
		return nil, fmt.Errorf("kg: raw refused: client is read-only")
	}
	res, err := c.db.SelectGraph(graph).Query(cypher, nil, c.queryOpts())
	if err != nil {
		return nil, fmt.Errorf("kg: raw on %s: %w", graph, err)
	}
	return rowsFrom(res)
}

func (c *Client) CopyGraph(ctx context.Context, src, dst string) error {
	if c.readOnly {
		return fmt.Errorf("kg: copy refused: client is read-only")
	}
	if !IsForkName(dst) {
		return fmt.Errorf("%w: %q lacks the %q prefix", ErrNotAFork, dst, ForkPrefix)
	}
	if err := c.db.Conn.Do(ctx, "GRAPH.COPY", src, dst).Err(); err != nil {
		return fmt.Errorf("kg: GRAPH.COPY %s -> %s: %w", src, dst, err)
	}
	return nil
}

func (c *Client) DropGraph(ctx context.Context, graph string) error {
	if c.readOnly {
		return fmt.Errorf("kg: drop refused: client is read-only")
	}
	if graph == c.graphName {
		return fmt.Errorf("%w: %q", ErrPrimaryGraph, graph)
	}
	if err := c.db.SelectGraph(graph).Delete(); err != nil {
		return fmt.Errorf("kg: drop %s: %w", graph, err)
	}
	return nil
}

func (c *Client) ListGraphs(ctx context.Context) ([]string, error) {
	names, err := c.db.ListGraphs()
	if err != nil {
		return nil, fmt.Errorf("kg: list graphs: %w", err)
	}
	return names, nil
}

func (c *Client) queryOpts() *falkordb.QueryOptions {
	return falkordb.NewQueryOptions().SetTimeout(int(c.timeout.Milliseconds()))
}

// ForkPrefix namespaces counterfactual graphs so the reaper can identify them
// and so a fork can never be mistaken for the primary corpus.
const ForkPrefix = "argus_cf_"

// IsForkName reports whether a graph key belongs to the counterfactual pool.
func IsForkName(s string) bool { return strings.HasPrefix(s, ForkPrefix) }

// redactURL strips credentials so connection errors are safe to log.
func redactURL(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return u
	}
	rest := u[i+3:]
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		return u[:i+3] + "***@" + rest[at+1:]
	}
	return u
}

var (
	_ Reader = (*Client)(nil)
	_ Writer = (*Client)(nil)
)

// Sentinel errors for guard conditions that callers and tests match on.
var (
	// ErrNotAFork is returned when a copy destination lacks the fork prefix, so
	// the reaper could never find it.
	ErrNotAFork = errors.New("kg: copy destination must use the fork prefix")
	// ErrPrimaryGraph is returned when something attempts to drop the corpus.
	ErrPrimaryGraph = errors.New("kg: refusing to drop the primary graph")
)
