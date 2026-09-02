package kg

import (
	"fmt"
	"math"
	"strconv"

	falkordb "github.com/FalkorDB/falkordb-go/v2"
)

// Row is one decoded result record, keyed by the query's RETURN aliases.
//
// The kg package decodes driver results into Rows at the boundary rather than
// handing *falkordb.QueryResult upward. Two reasons: callers stop depending on
// the driver's cursor semantics, and every engine above kg becomes testable
// against a fake that returns literal Rows - which is what lets the proof
// engine have real unit tests without a database.
type Row map[string]any

// Rows is an ordered result set.
type Rows []Row

// rowsFrom drains a driver result into Rows.
func rowsFrom(res *falkordb.QueryResult) (Rows, error) {
	if res == nil {
		return nil, nil
	}
	var out Rows
	for res.Next() {
		rec := res.Record()
		keys := rec.Keys()
		vals := rec.Values()
		if len(keys) != len(vals) {
			return nil, fmt.Errorf("kg: result has %d keys but %d values", len(keys), len(vals))
		}
		row := make(Row, len(keys))
		for i, k := range keys {
			row[k] = vals[i]
		}
		out = append(out, row)
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Typed accessors
//
// FalkorDB returns numbers as int64 or float64 depending on how the value was
// stored, and a property that was never set comes back as nil. These accessors
// absorb that so call sites do not each reinvent a type switch. Every one is
// total: a missing or wrong-typed field yields the zero value and a false, so a
// caller must decide explicitly whether absence is an error.
// ─────────────────────────────────────────────────────────────────────────────

// Str returns a string field.
func (r Row) Str(key string) (string, bool) {
	v, ok := r[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// StrOr returns a string field or a fallback.
func (r Row) StrOr(key, def string) string {
	if s, ok := r.Str(key); ok {
		return s
	}
	return def
}

// Int returns an integer field, accepting the int64/float64/string forms the
// driver may produce.
func (r Row) Int(key string) (int64, bool) {
	v, ok := r[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		// Reject a float that is not integral rather than silently truncating a
		// weight, which would corrupt a derivation's probability.
		if n != math.Trunc(n) {
			return 0, false
		}
		return int64(n), true
	case string:
		p, err := strconv.ParseInt(n, 10, 64)
		return p, err == nil
	}
	return 0, false
}

// IntOr returns an integer field or a fallback.
func (r Row) IntOr(key string, def int64) int64 {
	if n, ok := r.Int(key); ok {
		return n
	}
	return def
}

// Float returns a floating-point field.
func (r Row) Float(key string) (float64, bool) {
	v, ok := r[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case string:
		p, err := strconv.ParseFloat(n, 64)
		return p, err == nil
	}
	return 0, false
}

// FloatOr returns a floating-point field or a fallback.
func (r Row) FloatOr(key string, def float64) float64 {
	if f, ok := r.Float(key); ok {
		return f
	}
	return def
}

// Bool returns a boolean field.
func (r Row) Bool(key string) (bool, bool) {
	v, ok := r[key]
	if !ok || v == nil {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// Strings returns a list-of-strings field, as produced by a Cypher list
// comprehension such as `[n IN nodes(path) | n.id]`.
func (r Row) Strings(key string) ([]string, bool) {
	v, ok := r[key]
	if !ok || v == nil {
		return nil, false
	}
	switch xs := v.(type) {
	case []string:
		return xs, true
	case []any:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// Ints returns a list-of-integers field, such as per-step path weights.
func (r Row) Ints(key string) ([]int64, bool) {
	v, ok := r[key]
	if !ok || v == nil {
		return nil, false
	}
	xs, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]int64, 0, len(xs))
	for _, x := range xs {
		switch n := x.(type) {
		case int64:
			out = append(out, n)
		case int:
			out = append(out, int64(n))
		case float64:
			out = append(out, int64(n))
		default:
			return nil, false
		}
	}
	return out, true
}

// Floats returns a list-of-floats field, used for vectors read back out.
func (r Row) Floats(key string) ([]float64, bool) {
	v, ok := r[key]
	if !ok || v == nil {
		return nil, false
	}
	switch xs := v.(type) {
	case []float32:
		out := make([]float64, len(xs))
		for i, f := range xs {
			out[i] = float64(f)
		}
		return out, true
	case []float64:
		return xs, true
	case []any:
		out := make([]float64, 0, len(xs))
		for _, x := range xs {
			switch f := x.(type) {
			case float64:
				out = append(out, f)
			case float32:
				out = append(out, float64(f))
			case int64:
				out = append(out, float64(f))
			default:
				return nil, false
			}
		}
		return out, true
	}
	return nil, false
}

// VecParam converts a Go vector into the parameter form vecf32() accepts.
// FalkorDB requires every element to be a float, so integral values are widened
// explicitly rather than left as ints, which the server rejects.
func VecParam(v []float64) []any {
	out := make([]any, len(v))
	for i, f := range v {
		out[i] = f
	}
	return out
}

// StrParam converts a string slice into a parameter list for UNWIND.
func StrParam(v []string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}
