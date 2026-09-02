// Package extract turns source text into the claim graph.
//
// This is the most consequential package in ARGUS, because the proof engine can
// only ever be as trustworthy as the confidences written here. A path weight is
// -ln(confidence); if extraction hands out inflated confidences, every
// derivation looks stronger than it is and the abstention threshold stops
// meaning anything. Two mechanisms guard against that:
//
//   - Calibration by kind. A language model's self-reported confidence is not a
//     probability. Rather than trust it, ARGUS classifies each inference into a
//     small set of kinds with hard confidence ceilings, and the model's number
//     can only move a claim *down* within its kind's band, never above it.
//
//   - Span integrity. Every claim must quote the source text verbatim. A quote
//     that does not literally appear in the chunk is rejected at ingest, not
//     flagged later. A model cannot invent a fact and also invent evidence that
//     survives a substring check against the document it claims to be reading.
package extract

import (
	"fmt"
	"strings"
)

// EdgeKind classifies an inferential step, which fixes both its confidence
// ceiling and its leap cost.
//
// The ceilings are deliberately conservative. Over-confidence is the failure
// mode that silently breaks the whole system: it produces chains that clear the
// abstention floor without deserving to. Under-confidence merely makes ARGUS
// abstain more often, which is visible, measurable and safe.
type EdgeKind string

const (
	// KindStated: the premise text literally asserts the conclusion. The only
	// uncertainty is whether the source is right, not whether we read it right.
	KindStated EdgeKind = "stated"
	// KindDefinitional: the step follows from a definition, a threshold or
	// arithmetic ("a 60% holding is a controlling stake").
	KindDefinitional EdgeKind = "definitional"
	// KindInference: a well-supported but genuinely inferential step.
	KindInference EdgeKind = "inference"
	// KindAssumption: plausible, unstated, and the first thing an adversary
	// would attack. Priced accordingly.
	KindAssumption EdgeKind = "assumption"
)

// kindProfile fixes the confidence ceiling and leap cost for a kind.
type kindProfile struct {
	MaxConf float64
	Leap    int64
}

var kindProfiles = map[EdgeKind]kindProfile{
	KindStated:       {MaxConf: 0.98, Leap: 1},
	KindDefinitional: {MaxConf: 0.95, Leap: 1},
	KindInference:    {MaxConf: 0.80, Leap: 2},
	KindAssumption:   {MaxConf: 0.55, Leap: 3},
}

// Valid reports whether the kind is one ARGUS recognises.
func (k EdgeKind) Valid() bool {
	_, ok := kindProfiles[k]
	return ok
}

// Calibrate maps a model's self-reported confidence onto the kind's band.
//
// Nothing is ever raised: the kind's ceiling is a hard cap, and a model that
// declares 0.99 on an assumption gets 0.55. A floor of 0.05 keeps a weight
// finite. This is the single place where model output becomes a path weight,
// which is why it is a pure function with its own tests rather than something
// inlined into a prompt handler.
func (k EdgeKind) Calibrate(reported float64) float64 {
	p, ok := kindProfiles[k]
	if !ok {
		// An unrecognised kind is treated as the weakest possible step rather
		// than rejected, so a model inventing a label degrades the chain instead
		// of dropping evidence silently.
		p = kindProfiles[KindAssumption]
	}
	switch {
	case reported > p.MaxConf:
		return p.MaxConf
	case reported < 0.05:
		return 0.05
	default:
		return reported
	}
}

// LeapCost is the costProp value algo.SPpaths bounds with maxCost.
func (k EdgeKind) LeapCost() int64 {
	if p, ok := kindProfiles[k]; ok {
		return p.Leap
	}
	return kindProfiles[KindAssumption].Leap
}

// EntityType constrains what the extractor may produce, so the graph does not
// accumulate a long tail of one-off labels that no query knows about.
type EntityType string

const (
	EntityPerson  EntityType = "Person"
	EntityCompany EntityType = "Company"
	EntityFund    EntityType = "Fund"
	EntityAddress EntityType = "Address"
	EntityAccount EntityType = "Account"
)

var entityTypes = map[EntityType]bool{
	EntityPerson: true, EntityCompany: true, EntityFund: true,
	EntityAddress: true, EntityAccount: true,
}

func (t EntityType) Valid() bool { return entityTypes[t] }

// RelationType constrains entity-to-entity edges to the set the traversal
// queries actually follow. Adding one here without adding it to
// CandidateClaimsScoped would create edges the proof engine cannot see.
type RelationType string

const (
	RelOwns       RelationType = "OWNS"
	RelControls   RelationType = "CONTROLS"
	RelTransacted RelationType = "TRANSACTED_WITH"
)

var relationTypes = map[RelationType]bool{
	RelOwns: true, RelControls: true, RelTransacted: true,
}

func (t RelationType) Valid() bool { return relationTypes[t] }

// Entity is a party mentioned in the source text.
type Entity struct {
	Name string     `json:"name"`
	Type EntityType `json:"type"`
	// Quote is the verbatim text the entity was read from, used for span
	// integrity checking.
	Quote string `json:"quote"`
}

// Claim is a single assertion extracted from a chunk.
type Claim struct {
	// Ref is a chunk-local identifier the model uses to wire up Entails edges.
	// It is never persisted; the ingester assigns stable graph IDs.
	Ref string `json:"ref"`
	// Text is the assertion, stated as a self-contained sentence so it can be
	// read on its own inside a proof chain.
	Text string `json:"text"`
	// Quote must appear verbatim in the source chunk.
	Quote string `json:"quote"`
	// Entities names the parties this claim is about; they must appear in the
	// extraction's Entities list.
	Entities []string `json:"entities"`
	// Confidence is the model's self-report, before calibration.
	Confidence float64 `json:"confidence"`
	// ValidFrom and ValidTo bound when the claim holds, as RFC3339 dates or "".
	// An empty ValidTo means "still current", which is what makes a temporal
	// query answerable.
	ValidFrom string `json:"validFrom"`
	ValidTo   string `json:"validTo"`
}

// Entailment is an inference edge between two claims in the same chunk.
type Entailment struct {
	From string   `json:"from"` // Claim.Ref
	To   string   `json:"to"`   // Claim.Ref
	Kind EdgeKind `json:"kind"`
	// Confidence is the model's self-report for this step, before calibration.
	Confidence float64 `json:"confidence"`
	// Rationale is one clause explaining the step, shown in the proof chain UI.
	Rationale string `json:"rationale"`
}

// Relation is an entity-to-entity edge.
type Relation struct {
	From   string       `json:"from"` // Entity.Name
	To     string       `json:"to"`
	Type   RelationType `json:"type"`
	Amount float64      `json:"amount"` // percentage or value; 0 when not stated
	Quote  string       `json:"quote"`
}

// Extraction is everything read out of one chunk.
type Extraction struct {
	Entities  []Entity     `json:"entities"`
	Claims    []Claim      `json:"claims"`
	Entails   []Entailment `json:"entails"`
	Relations []Relation   `json:"relations"`
}

// Rejection records why a candidate was discarded. Rejections are counted and
// reported rather than logged and forgotten: a corpus where 40% of claims fail
// span integrity is telling you the extraction prompt is broken, and that
// should be visible in the ingest summary.
type Rejection struct {
	Kind   string // span | entity | ref | type | empty
	Detail string
	Quote  string
}

func (r Rejection) String() string {
	q := r.Quote
	if len(q) > 60 {
		q = q[:60] + "..."
	}
	return fmt.Sprintf("%s: %s (%q)", r.Kind, r.Detail, q)
}

// Report summarises one extraction pass.
type Report struct {
	ChunksProcessed int
	EntitiesKept    int
	ClaimsKept      int
	EntailsKept     int
	RelationsKept   int
	Rejections      []Rejection
}

// RejectionRate is the share of candidate claims that failed validation. A
// healthy corpus sits low; a high rate means the prompt or the chunking is
// wrong, and the ingester surfaces it rather than quietly thinning the graph.
func (r Report) RejectionRate() float64 {
	total := r.ClaimsKept + len(r.Rejections)
	if total == 0 {
		return 0
	}
	return float64(len(r.Rejections)) / float64(total)
}

func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d chunks -> %d entities, %d claims, %d inference edges, %d relations",
		r.ChunksProcessed, r.EntitiesKept, r.ClaimsKept, r.EntailsKept, r.RelationsKept)
	if len(r.Rejections) > 0 {
		fmt.Fprintf(&b, "; %d rejected (%.0f%%)", len(r.Rejections), r.RejectionRate()*100)
	}
	return b.String()
}

// Merge folds another report into this one.
func (r *Report) Merge(o Report) {
	r.ChunksProcessed += o.ChunksProcessed
	r.EntitiesKept += o.EntitiesKept
	r.ClaimsKept += o.ClaimsKept
	r.EntailsKept += o.EntailsKept
	r.RelationsKept += o.RelationsKept
	r.Rejections = append(r.Rejections, o.Rejections...)
}
