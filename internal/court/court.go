// Package court implements ARGUS Feature C: dispute adjudication.
//
// Real corpora disagree. A conventional RAG system silently picks whichever
// contradictory passage ranked higher and never tells the user a dispute
// existed - which is the single most dangerous failure mode in an
// investigation, because the answer looks exactly as confident as a true one.
//
// ARGUS treats disagreement as structure, and structure has measurable
// properties:
//
//   - Credibility is a network property. Source trust comes from PageRank over
//     the citation graph, not from a constant an engineer typed in, so a source
//     nothing corroborates cannot bootstrap its own authority.
//   - Corroboration is a flow property. algo.maxFlow from origin publishers to a
//     claim measures how much *independent* support exists. Ten outlets that
//     all republish one press release yield the flow of one source, because
//     every path is bottlenecked by the shared origin. Counting them yields ten.
//   - A dispute is an edge. Surfacing it is a lookup, not an inference.
//
// The gap between the direct source count and the max flow is the finding: "six
// reports, one origin" is a real journalistic insight that a vector database
// structurally cannot produce.
package court

import (
	"context"
	"fmt"
	"sort"

	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/prove"
)

// Stance is which side of a dispute a source takes.
type Stance string

const (
	StanceSupports Stance = "supports"
	StanceDisputes Stance = "disputes"
)

// SourceRef is a publisher's position on a claim.
type SourceRef struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`
	Trust float64 `json:"trust"`
}

// Dispute is one contradicted claim on a proof chain.
type Dispute struct {
	ClaimID string `json:"claimId"`
	// CounterText is the competing assertion.
	CounterText string  `json:"counterText"`
	CounterConf float64 `json:"counterConfidence"`
	// CounterSource is who makes it.
	CounterSource SourceRef `json:"counterSource"`
	// DetectedBy records how the contradiction was identified, so a false
	// positive can be traced back to the detector rather than argued about.
	DetectedBy string `json:"detectedBy"`
}

// Corroboration is the independence analysis for one claim.
type Corroboration struct {
	ClaimID string `json:"claimId"`
	// DirectSources is how many publishers assert the claim, counting naively.
	DirectSources int `json:"directSources"`
	// IndependentFlow is the max-flow from origin publishers, in trust units.
	// It is bounded by the narrowest cut, so shared upstreams do not inflate it.
	IndependentFlow int64 `json:"independentFlow"`
	// OriginCount is how many publishers cite nobody - the roots of the evidence.
	OriginCount int `json:"originCount"`
	// Bottlenecked is true when many sources resolve to little independent flow.
	Bottlenecked bool `json:"bottlenecked"`
	// Note is the human-readable finding, shown verbatim in the UI.
	Note string `json:"note,omitempty"`
	// Sources lists the direct supporters.
	Sources []SourceRef `json:"sources"`
}

// Ruling is the court's verdict on a proof chain.
type Ruling struct {
	// Contested is true when any claim on the chain is disputed.
	Contested     bool            `json:"contested"`
	Disputes      []Dispute       `json:"disputes"`
	Corroboration []Corroboration `json:"corroboration"`
	// TrustRanking is the PageRank ordering of publishers, for the UI panel.
	TrustRanking []SourceRef `json:"trustRanking,omitempty"`
	GraphMS      int64       `json:"graphMs"`
}

// Engine adjudicates disputes.
type Engine struct {
	graph kg.Reader
	// MaxClaims bounds how many chain claims get a corroboration analysis.
	// maxFlow is O(V*E^2), so this is a real cost ceiling rather than a
	// tidiness limit.
	MaxClaims int
}

func NewEngine(graph kg.Reader) *Engine {
	return &Engine{graph: graph, MaxClaims: 6}
}

// Adjudicate examines a verdict's strongest chain for disputes and measures how
// independently corroborated its claims are.
func (e *Engine) Adjudicate(ctx context.Context, v *prove.Verdict) (*Ruling, error) {
	r := &Ruling{}
	if v == nil || len(v.Chains) == 0 {
		return r, nil
	}

	claimIDs := chainClaims(v, e.MaxClaims)
	if len(claimIDs) == 0 {
		return r, nil
	}

	disputes, err := e.disputes(ctx, claimIDs)
	if err != nil {
		return nil, err
	}
	r.Disputes = disputes
	r.Contested = len(disputes) > 0

	for _, id := range claimIDs {
		c, err := e.corroboration(ctx, id)
		if err != nil {
			// A corroboration failure must not sink the ruling: the disputes are
			// still worth reporting, and an incomplete analysis is better than
			// none. It is visible because the claim simply has no entry.
			continue
		}
		r.Corroboration = append(r.Corroboration, c)
	}
	return r, nil
}

// Status folds a ruling into the verdict status the UI renders.
func (r *Ruling) Status(base prove.Status) prove.Status {
	if base == prove.StatusProven && r.Contested {
		return prove.StatusContested
	}
	return base
}

func (e *Engine) disputes(ctx context.Context, claimIDs []string) ([]Dispute, error) {
	rows, err := e.graph.Read(ctx, kg.CourtContradictions, kg.Params{
		"claimIDs": kg.StrParam(claimIDs),
	})
	if err != nil {
		return nil, fmt.Errorf("court: contradictions: %w", err)
	}
	out := make([]Dispute, 0, len(rows))
	for _, row := range rows {
		id, ok := row.Str("claimID")
		if !ok {
			continue
		}
		out = append(out, Dispute{
			ClaimID:     id,
			CounterText: row.StrOr("counterText", ""),
			CounterConf: row.FloatOr("counterConf", 0),
			DetectedBy:  row.StrOr("detectedBy", "unknown"),
			CounterSource: SourceRef{
				ID:    row.StrOr("sourceID", ""),
				Name:  row.StrOr("sourceName", ""),
				Trust: row.FloatOr("sourceTrust", 0),
			},
		})
	}
	return out, nil
}

func (e *Engine) corroboration(ctx context.Context, claimID string) (Corroboration, error) {
	c := Corroboration{ClaimID: claimID}

	direct, err := e.graph.Read(ctx, kg.CourtDirectSupport, kg.Params{"claimID": claimID})
	if err != nil {
		return c, fmt.Errorf("court: direct support for %s: %w", claimID, err)
	}
	for _, row := range direct {
		c.Sources = append(c.Sources, SourceRef{
			ID:    row.StrOr("id", ""),
			Name:  row.StrOr("name", ""),
			Kind:  row.StrOr("kind", ""),
			Trust: row.FloatOr("trust", 0),
		})
	}
	c.DirectSources = len(c.Sources)

	flow, err := e.graph.Read(ctx, kg.CourtCorroboration, kg.Params{"claimID": claimID})
	if err != nil {
		return c, fmt.Errorf("court: corroboration flow for %s: %w", claimID, err)
	}
	if len(flow) > 0 {
		c.IndependentFlow = flow[0].IntOr("flow", 0)
		c.OriginCount = int(flow[0].IntOr("originCount", 0))
	}

	c.Bottlenecked, c.Note = interpret(c)
	return c, nil
}

// interpret turns the numbers into the sentence a reader acts on.
//
// The threshold is deliberately conservative. A bottleneck warning that fires
// on ordinary evidence would train users to ignore it, which is worse than not
// warning at all.
func interpret(c Corroboration) (bool, string) {
	if c.DirectSources <= 1 {
		if c.DirectSources == 1 {
			return false, fmt.Sprintf("Single source: %s. Nothing corroborates this.",
				c.Sources[0].Name)
		}
		return false, ""
	}

	// Expected flow if every source were independent: each contributes its own
	// trust-scaled capacity.
	var expected int64
	for _, s := range c.Sources {
		expected += int64(s.Trust * 100)
	}
	if expected == 0 || c.IndependentFlow == 0 {
		return false, ""
	}

	ratio := float64(c.IndependentFlow) / float64(expected)
	if ratio >= 0.6 {
		return false, fmt.Sprintf("%d independent sources.", c.DirectSources)
	}
	return true, fmt.Sprintf(
		"%d sources assert this, but they carry only %.0f%% of the independent "+
			"support that %d unrelated sources would. They share an upstream origin.",
		c.DirectSources, ratio*100, c.DirectSources)
}

// TrustRanking returns the PageRank ordering of publishers.
func (e *Engine) TrustRanking(ctx context.Context, limit int64) ([]SourceRef, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := e.graph.Read(ctx, kg.CourtSourceTrust, kg.Params{"limit": limit})
	if err != nil {
		return nil, fmt.Errorf("court: trust ranking: %w", err)
	}
	out := make([]SourceRef, 0, len(rows))
	for _, row := range rows {
		out = append(out, SourceRef{
			ID:    row.StrOr("id", ""),
			Name:  row.StrOr("name", ""),
			Kind:  row.StrOr("kind", ""),
			Trust: row.FloatOr("score", 0),
		})
	}
	return out, nil
}

// Materialise builds the corroboration flow network.
//
// Run once after ingest. Capacities depend on trust, which depends on the whole
// citation graph, so these edges cannot be written during ingestion.
func Materialise(ctx context.Context, g kg.Writer) error {
	if _, err := g.Write(ctx, kg.MaterialiseCitedBy, nil); err != nil {
		return fmt.Errorf("court: materialising citation edges: %w", err)
	}
	if _, err := g.Write(ctx, kg.MaterialiseCorroboration, nil); err != nil {
		return fmt.Errorf("court: materialising corroboration edges: %w", err)
	}
	return nil
}

// chainClaims collects distinct claim ids across a verdict's chains.
func chainClaims(v *prove.Verdict, limit int) []string {
	seen := map[string]bool{}
	var out []string
	for _, ch := range v.Chains {
		for _, s := range ch.Steps {
			if seen[s.ClaimID] {
				continue
			}
			seen[s.ClaimID] = true
			out = append(out, s.ClaimID)
			if limit > 0 && len(out) >= limit {
				sort.Strings(out)
				return out
			}
		}
	}
	sort.Strings(out)
	return out
}
