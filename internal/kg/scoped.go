package kg

import "context"

// Scoped returns a Reader whose unqualified reads target a specific graph key.
//
// The counterfactual engine needs to run an unmodified proof search against a
// fork. Rather than teaching every engine about graph names - which would push
// fork awareness through the whole call stack for one feature's benefit - the
// fork is presented as an ordinary Reader that happens to point elsewhere.
//
// The wrapper is read-only by construction: it does not implement Writer, so a
// scoped handle cannot mutate anything, in a fork or otherwise.
func Scoped(r Reader, graph string) Reader {
	return &scopedReader{inner: r, graph: graph}
}

type scopedReader struct {
	inner Reader
	graph string
}

func (s *scopedReader) Read(ctx context.Context, t Template, p Params) (Rows, error) {
	return s.inner.ReadGraph(ctx, s.graph, t, p)
}

// ReadGraph ignores the scope: an explicit graph name is always honoured, so a
// caller that knows what it wants is never silently redirected.
func (s *scopedReader) ReadGraph(ctx context.Context, graph string, t Template, p Params) (Rows, error) {
	return s.inner.ReadGraph(ctx, graph, t, p)
}

func (s *scopedReader) Plan(ctx context.Context, t Template, p Params) (string, error) {
	return s.inner.Plan(ctx, t, p)
}

func (s *scopedReader) GraphName() string { return s.graph }

// Close is a no-op: the scoped view does not own the underlying connection, and
// closing it would take the primary client down with it.
func (s *scopedReader) Close() error { return nil }

var _ Reader = (*scopedReader)(nil)
