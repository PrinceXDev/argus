// Package retract implements ARGUS Feature B: counterfactual retraction.
//
// The question it answers is one no conventional RAG system can even pose:
// *which single fact is this conclusion actually resting on?*
//
// Attribution tells you what was retrieved. It does not tell you what was
// necessary. Necessity is a counterfactual, and a counterfactual needs a second
// world to evaluate in. FalkorDB provides one cheaply: GRAPH.COPY duplicates a
// graph under a new key while the source stays fully readable, so ARGUS can
// fork, ablate one claim, re-derive, and diff - without touching the corpus
// anyone else is querying.
//
// Two properties are worth stating plainly because they are what make this
// feature defensible rather than merely flashy:
//
//   - No language model is involved. The question vector is computed once, by
//     the caller, and every re-derivation is pure graph computation. The
//     alternative - leave-one-chunk-out re-prompting - costs one model call per
//     candidate and measures the model's sensitivity rather than the evidence's
//     necessity.
//   - Ablation zeroes confidence and cuts inference edges rather than deleting
//     the node. That measures the effect of *disbelieving* a claim, not the
//     effect of a structural hole, which are different counterfactuals.
package retract

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/prove"
)

// Impact classifies how much a conclusion depends on one claim.
type Impact string

const (
	// ImpactCritical: retracting the claim collapses the answer entirely - the
	// system would abstain without it.
	ImpactCritical Impact = "critical"
	// ImpactMajor: the answer survives but confidence falls substantially.
	ImpactMajor Impact = "major"
	// ImpactMinor: measurable but small effect.
	ImpactMinor Impact = "minor"
	// ImpactNone: the conclusion is derivable by another route of equal
	// strength. The claim is corroborated, not load-bearing.
	ImpactNone Impact = "none"
)

// Retraction is the measured effect of removing one claim.
type Retraction struct {
	ClaimID string `json:"claimId"`
	Text    string `json:"text"`

	// BaseConfidence is the derivation probability with the claim present.
	BaseConfidence float64 `json:"baseConfidence"`
	// Confidence is the probability after retracting it.
	Confidence float64 `json:"confidence"`
	// Delta is how much probability the claim contributes.
	Delta float64 `json:"delta"`

	// Collapses is true when retraction pushes the system into abstention -
	// whether because no derivation survives at all, or because the surviving
	// one falls below the confidence floor. Both mean the same thing to a user:
	// without this claim, ARGUS would decline to answer.
	Collapses bool   `json:"collapses"`
	Impact    Impact `json:"impact"`

	// AlternativeHops is the length of the best surviving derivation, or 0 when
	// none survives. A longer alternative means the corpus can still reach the
	// answer, but by a weaker route.
	AlternativeHops int64 `json:"alternativeHops"`
}

// Analysis is the ranked result of a retraction sweep.
type Analysis struct {
	Question       string       `json:"question"`
	BaseConfidence float64      `json:"baseConfidence"`
	Retractions    []Retraction `json:"retractions"`

	// LoadBearing names the claims whose removal collapses the answer. This is
	// the sentence the demo lands on: "this conclusion rested on one filing."
	LoadBearing []string `json:"loadBearing"`

	ForksUsed int   `json:"forksUsed"`
	GraphMS   int64 `json:"graphMs"`
	// LLMCalls is always zero, and is reported precisely so that is visible.
	LLMCalls int `json:"llmCalls"`
}

// Engine runs counterfactual retraction sweeps.
type Engine struct {
	graph  kg.Writer
	prover *prove.Engine

	// Concurrency bounds simultaneous forks. Each fork is a full graph copy, so
	// this is a memory dial as much as a speed one: forks are separate graph
	// keys and so their reads parallelise, but N forks cost N times the graph's
	// resident size.
	Concurrency int
	// MaxCandidates bounds how many claims are tested. Sweeping every claim in a
	// large derivation would be quadratic in demo time for no extra insight -
	// the load-bearing fact is almost always in the top few.
	MaxCandidates int
}

func NewEngine(graph kg.Writer, prover *prove.Engine) *Engine {
	return &Engine{graph: graph, prover: prover, Concurrency: 3, MaxCandidates: 12}
}

// Analyse measures how much each claim on a verdict's chains matters.
//
// The caller supplies the already-computed question vector so no model call
// happens anywhere in this path.
func (e *Engine) Analyse(ctx context.Context, q prove.Question, qvec []float64, base *prove.Verdict) (*Analysis, error) {
	if base == nil || len(base.Chains) == 0 {
		return nil, errors.New("retract: nothing to analyse - the verdict has no chains")
	}
	if len(qvec) == 0 {
		return nil, errors.New("retract: a precomputed question vector is required")
	}
	start := time.Now()

	baseConf := base.Chains[0].Confidence
	candidates := candidateClaims(base, e.MaxCandidates)
	if len(candidates) == 0 {
		return nil, errors.New("retract: no candidate claims on the verdict's chains")
	}

	out := &Analysis{
		Question:       q.Text,
		BaseConfidence: baseConf,
		ForksUsed:      len(candidates),
	}

	results := make([]Retraction, len(candidates))
	errs := make([]error, len(candidates))
	sem := make(chan struct{}, max(1, e.Concurrency))
	var wg sync.WaitGroup

	for i, c := range candidates {
		wg.Add(1)
		go func(i int, c candidate) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()

			r, err := e.testOne(ctx, q, qvec, baseConf, c, i)
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = r
		}(i, c)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	// Most impactful first: that is the ordering the UI reads top-down and the
	// only one in which the headline finding is the first row.
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Collapses != results[j].Collapses {
			return results[i].Collapses
		}
		return results[i].Delta > results[j].Delta
	})
	out.Retractions = results
	for _, r := range results {
		if r.Collapses {
			out.LoadBearing = append(out.LoadBearing, r.ClaimID)
		}
	}
	out.GraphMS = time.Since(start).Milliseconds()
	return out, nil
}

type candidate struct {
	id   string
	text string
}

// candidateClaims collects the distinct claims across a verdict's chains,
// preserving the order they appear in the strongest chain first.
func candidateClaims(v *prove.Verdict, limit int) []candidate {
	seen := map[string]bool{}
	var out []candidate
	for _, ch := range v.Chains {
		for _, s := range ch.Steps {
			if seen[s.ClaimID] {
				continue
			}
			seen[s.ClaimID] = true
			out = append(out, candidate{id: s.ClaimID, text: s.Text})
			if limit > 0 && len(out) >= limit {
				return out
			}
		}
	}
	return out
}

// testOne forks the graph, ablates one claim, and re-derives.
func (e *Engine) testOne(ctx context.Context, q prove.Question, qvec []float64,
	baseConf float64, c candidate, slot int) (Retraction, error) {

	fork := fmt.Sprintf("%s%d_%s", kg.ForkPrefix, slot, shortID(c.id))

	// A leftover fork from an aborted run would be re-used with stale state, so
	// the slot is always cleared first.
	_ = e.graph.DropGraph(ctx, fork)

	if err := e.graph.CopyGraph(ctx, e.graph.GraphName(), fork); err != nil {
		return Retraction{}, fmt.Errorf("retract: forking for %s: %w", c.id, err)
	}
	// The fork is dropped whatever happens, including on a panic in the
	// re-derivation, so a failed sweep cannot leak graphs into the instance.
	defer func() {
		if err := e.graph.DropGraph(context.WithoutCancel(ctx), fork); err != nil {
			// Reaping is best-effort; the startup sweeper is the backstop.
			_ = err
		}
	}()

	if _, err := e.graph.WriteGraph(ctx, fork, kg.RetractClaim, kg.Params{
		"claimID": c.id,
	}); err != nil {
		return Retraction{}, fmt.Errorf("retract: ablating %s in %s: %w", c.id, fork, err)
	}

	// Re-derive against the fork using an unmodified proof engine, pointed
	// elsewhere by a read-only scoped view.
	forked := prove.NewEngine(kg.Scoped(e.graph, fork), nil)
	forked.Concurrency = 4
	v, err := forked.AnswerWithVector(ctx, q, qvec)
	if err != nil {
		return Retraction{}, fmt.Errorf("retract: re-deriving without %s: %w", c.id, err)
	}

	r := Retraction{
		ClaimID:        c.id,
		Text:           c.text,
		BaseConfidence: baseConf,
	}
	if best := v.Best(); best != nil && v.Status != prove.StatusInsufficient {
		r.Confidence = best.Confidence
		r.AlternativeHops = best.Hops
	}
	r.Delta = baseConf - r.Confidence
	if r.Delta < 0 {
		// Removing evidence should never strengthen a conclusion. If it appears
		// to, the surviving route was simply not the one originally chosen;
		// reporting a negative contribution would be misleading.
		r.Delta = 0
	}
	r.Collapses = v.Status == prove.StatusInsufficient
	r.Impact = classify(r)
	return r, nil
}

func classify(r Retraction) Impact {
	switch {
	case r.Collapses:
		return ImpactCritical
	case r.BaseConfidence > 0 && r.Delta/r.BaseConfidence >= 0.30:
		return ImpactMajor
	case r.Delta > 0.01:
		return ImpactMinor
	default:
		return ImpactNone
	}
}

// ReapForks deletes every leftover counterfactual graph.
//
// Forks are named with a reserved prefix precisely so an orphan from a crashed
// run can be found and removed without a registry. Called at startup.
func ReapForks(ctx context.Context, g kg.Writer) (int, error) {
	names, err := g.ListGraphs(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, name := range names {
		if !kg.IsForkName(name) {
			continue
		}
		if err := g.DropGraph(ctx, name); err != nil {
			return n, fmt.Errorf("retract: reaping %s: %w", name, err)
		}
		n++
	}
	return n, nil
}

func shortID(s string) string {
	if len(s) <= 12 {
		return sanitise(s)
	}
	return sanitise(s[len(s)-12:])
}

// sanitise keeps a fork key to characters that are unambiguous in a Redis key.
func sanitise(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}
