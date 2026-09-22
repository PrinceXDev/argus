package kg

import (
	"context"
	"fmt"
	"math"
	"strings"
)

// The ARGUS graph schema.
//
// The central modelling decision is that ARGUS stores Claim nodes, not just
// Chunk and Entity nodes. A conventional GraphRAG schema connects entities to
// each other, so a path through the graph is a chain of *associations*. ARGUS
// connects assertions to each other via ENTAILS, so a path through the graph is
// a chain of *inference* - a derivation. That is what makes a returned path an
// answer's proof rather than a list of things that happen to be related.
//
// Two numeric properties on ENTAILS carry the reasoning:
//
//	w_int  fixed-point -ln(confidence), minimised by algo.SPpaths via weightProp
//	leap   inferential-leap units, bounded by algo.SPpaths via costProp/maxCost
//
// Because sum(-ln p_i) = -ln(prod p_i), the minimum-weight path is exactly the
// maximum-likelihood derivation. The separate leap budget lets a user ask for a
// less speculative answer without changing what "most likely" means.

// WeightScale converts confidence to the fixed-point integer used by
// weightProp. FalkorDB documents pathWeight as an integer, so ARGUS encodes
// weights as milli-nats rather than depending on float weight support.
const WeightScale = 1000

// MinConfidence floors confidence before taking a logarithm, so a zero or
// near-zero confidence produces a large but finite weight instead of +Inf.
const MinConfidence = 1e-6

// WeightFromConfidence returns round(-ln(conf) * WeightScale).
// conf is clamped to (0, 1]. A confidence of 1.0 yields weight 0: a certain
// step costs nothing to traverse.
func WeightFromConfidence(conf float64) int64 {
	if conf > 1 {
		conf = 1
	}
	if conf < MinConfidence {
		conf = MinConfidence
	}
	return int64(math.Round(-math.Log(conf) * WeightScale))
}

// ConfidenceFromWeight inverts WeightFromConfidence, recovering the probability
// of a whole derivation from the pathWeight that algo.SPpaths returns.
func ConfidenceFromWeight(w int64) float64 {
	return math.Exp(-float64(w) / WeightScale)
}

// schemaDDL is the full set of constraints and indexes.
//
// Constraints are not decoration. The MANDATORY constraints below make an
// unsourced claim *unrepresentable*: a Claim without a confidence and an
// ASSERTED_BY edge without a source span cannot be written at all. "Every claim
// carries its source" is therefore a database invariant rather than an
// application convention, and it holds even against a buggy ingester.
func schemaDDL(embedDim int) []ddl {
	vec := func(pattern, prop string) string {
		return fmt.Sprintf(
			"CREATE VECTOR INDEX FOR %s ON (%s) OPTIONS {dimension:%d, similarityFunction:'cosine', M:32, efConstruction:200}",
			pattern, prop, embedDim)
	}
	return []ddl{
		// --- Range indexes: entry points and time-scoped traversal ------------
		{"idx.entity.id", "CREATE INDEX FOR (e:Entity) ON (e.id)"},
		{"idx.claim.id", "CREATE INDEX FOR (c:Claim) ON (c.id)"},
		{"idx.claim.valid_from", "CREATE INDEX FOR (c:Claim) ON (c.valid_from)"},
		{"idx.claim.valid_to", "CREATE INDEX FOR (c:Claim) ON (c.valid_to)"},
		{"idx.document.id", "CREATE INDEX FOR (d:Document) ON (d.id)"},
		{"idx.source.id", "CREATE INDEX FOR (s:Source) ON (s.id)"},

		// --- Full-text: keyword anchoring -------------------------------------
		{"ft.claim.text", "CALL db.idx.fulltext.createNodeIndex('Claim', 'text')"},
		{"ft.entity.name", "CALL db.idx.fulltext.createNodeIndex('Entity', 'canonical_name')"},

		// --- Vector: semantic anchoring and scoped ranking --------------------
		// The ANN index serves the global entry point. Inside a traversal-scoped
		// candidate set ARGUS uses vec.cosineDistance directly, because FalkorDB
		// documents that vector index queries do not compose with property
		// filters - and because a graph-scoped candidate set is small enough for
		// exact cosine to be both cheaper and more accurate than ANN.
		{"vec.chunk", vec("(c:Chunk)", "c.embedding")},
		{"vec.claim", vec("(c:Claim)", "c.embedding")},
		{"vec.entity", vec("(e:Entity)", "e.embedding")},
	}
}

// constraintDDL uses GRAPH.CONSTRAINT, which is a command rather than Cypher,
// so it is issued separately from the index DDL.
var constraintDDL = []constraint{
	{"claim.conf", "MANDATORY", "NODE", "Claim", []string{"conf"}},
	{"claim.id", "UNIQUE", "NODE", "Claim", []string{"id"}},
	{"entity.id", "UNIQUE", "NODE", "Entity", []string{"id"}},
	{"source.id", "UNIQUE", "NODE", "Source", []string{"id"}},
	// An assertion edge with no character span is an assertion with no evidence.
	{"asserted_by.span", "MANDATORY", "RELATIONSHIP", "ASSERTED_BY", []string{"span"}},
	// An inference edge with no weight cannot participate in a derivation.
	{"entails.w_int", "MANDATORY", "RELATIONSHIP", "ENTAILS", []string{"w_int"}},
	{"entails.leap", "MANDATORY", "RELATIONSHIP", "ENTAILS", []string{"leap"}},
}

type ddl struct {
	name   string
	cypher string
}

type constraint struct {
	name       string
	kind       string // MANDATORY | UNIQUE
	entityType string // NODE | RELATIONSHIP
	label      string
	props      []string
}

// EnsureSchema creates every index and constraint. It is idempotent: an
// "already indexed" or "already exists" error from FalkorDB is treated as
// success, so the ingester can call it on every run.
func EnsureSchema(ctx context.Context, w Writer, embedDim int) (SchemaReport, error) {
	var rep SchemaReport
	graph := w.GraphName()

	for _, d := range schemaDDL(embedDim) {
		if _, err := w.Raw(ctx, graph, d.cypher); err != nil {
			if isAlreadyExists(err) {
				rep.Existing = append(rep.Existing, d.name)
				continue
			}
			return rep, fmt.Errorf("schema: %s: %w", d.name, err)
		}
		rep.Created = append(rep.Created, d.name)
	}

	cw, ok := w.(*Client)
	if !ok {
		return rep, nil
	}

	for _, c := range constraintDDL {
		args := []interface{}{"GRAPH.CONSTRAINT", "CREATE", graph, c.kind, c.entityType, c.label,
			"PROPERTIES", len(c.props)}
		for _, p := range c.props {
			args = append(args, p)
		}
		if err := cw.db.Conn.Do(ctx, args...).Err(); err != nil {
			if isAlreadyExists(err) {
				rep.Existing = append(rep.Existing, "constraint:"+c.name)
				continue
			}
			return rep, fmt.Errorf("schema: constraint %s: %w", c.name, err)
		}
		rep.Created = append(rep.Created, "constraint:"+c.name)
	}
	return rep, nil
}

// SchemaReport records what EnsureSchema did, so `argus-ingest --schema-only`
// can print an honest summary.
type SchemaReport struct {
	Created  []string
	Existing []string
}

func (r SchemaReport) String() string {
	return fmt.Sprintf("schema: %d created, %d already present", len(r.Created), len(r.Existing))
}

// isAlreadyExists recognises FalkorDB's idempotency errors. The server does not
// return typed errors, so this matches on message text; the set of phrases is
// deliberately narrow so a real failure is never swallowed.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "already indexed") ||
		strings.Contains(m, "already exists") ||
		strings.Contains(m, "attempted to create an existing")
}
