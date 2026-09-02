// Package prove implements ARGUS Feature A: proof-carrying answers.
//
// The central idea is that finding the best explanation for a question is a
// shortest-path problem, and FalkorDB solves it natively.
//
// Each ENTAILS edge between claims carries w_int, the fixed-point value of
// -ln(confidence). Because sum(-ln p_i) = -ln(prod p_i), the total weight of a
// path is -ln of the joint probability of every inferential step along it, so
// the minimum-weight path from a grounded premise to a candidate answer is
// exactly the maximum-likelihood derivation. No heuristic, no reranker: the
// database returns the best explanation because "best" was encoded as
// "shortest" before the query ran.
//
// A second, independent axis - `leap`, bounded by maxCost - counts inferential
// leaps. It lets a caller demand a less speculative answer without changing
// what "most likely" means. Two knobs, one procedure call.
//
// The language model is never asked to reason. It embeds the question, and at
// the end it reads the returned chain out loud. Everything between those two
// points is graph computation, which is what makes the result auditable and
// what makes an abstention trustworthy: when no path exists inside the budget,
// that is a fact about the corpus, not a mood of the model.
package prove

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/llm"
)

// Status is the outcome of a proof search.
type Status string

const (
	// StatusProven means a derivation was found inside the budget and nothing
	// on it is contested.
	StatusProven Status = "proven"
	// StatusContested means a derivation was found but at least one claim on it
	// is contradicted by another sourced claim.
	StatusContested Status = "contested"
	// StatusInsufficient means no derivation exists inside the budget. This is a
	// result, not an error: it is the answer a vector store cannot give, because
	// a top-k search always returns something.
	StatusInsufficient Status = "insufficient_evidence"
)

// Question is a request for a proven answer.
type Question struct {
	// Text is the natural-language question.
	Text string
	// AsOf scopes the graph to claims valid at this instant, so a later
	// restatement cannot answer a question about an earlier state of the world.
	AsOf time.Time
	// LeapBudget bounds total inferential leaps along a chain. This is the
	// speculation slider: lower means the system demands more direct evidence
	// and abstains sooner.
	LeapBudget int64
	// MaxHops bounds chain length.
	MaxHops int64
	// MinConfidence is the derivation probability below which ARGUS abstains
	// rather than answering.
	MinConfidence float64
	// MaxRoots and MaxCandidates bound the search. Both cost one query per unit
	// in the worst case, so they are the latency dial.
	MaxRoots      int64
	MaxCandidates int64
	// MaxSemanticDistance filters candidate claims that are structurally
	// reachable but semantically unrelated to the question.
	MaxSemanticDistance float64
}

// Defaults fills unset fields with values tuned for the investigation corpus.
func (q Question) Defaults() Question {
	if q.AsOf.IsZero() {
		q.AsOf = time.Now()
	}
	if q.LeapBudget == 0 {
		q.LeapBudget = 6
	}
	if q.MaxHops == 0 {
		q.MaxHops = 6
	}
	if q.MinConfidence == 0 {
		q.MinConfidence = 0.35
	}
	if q.MaxRoots == 0 {
		q.MaxRoots = 8
	}
	if q.MaxCandidates == 0 {
		q.MaxCandidates = 12
	}
	if q.MaxSemanticDistance == 0 {
		q.MaxSemanticDistance = 0.65
	}
	return q
}

// Step is one inferential hop in a chain.
type Step struct {
	ClaimID string  `json:"claimId"`
	Text    string  `json:"text"`
	Weight  int64   `json:"weight"`     // w_int of the edge that reached this claim
	Conf    float64 `json:"confidence"` // that edge's confidence
}

// Chain is one complete derivation.
type Chain struct {
	// Confidence is the recovered joint probability, exp(-weight/scale).
	Confidence float64 `json:"confidence"`
	// Weight is the raw fixed-point path weight from FalkorDB.
	Weight int64 `json:"weight"`
	// Leaps is pathCost: how speculative this chain is.
	Leaps int64  `json:"leaps"`
	Hops  int64  `json:"hops"`
	Steps []Step `json:"steps"`
}

// FrontierClaim is a claim at the edge of what the corpus can derive. Returned
// on abstention so a refusal points at the missing evidence instead of just
// declining.
type FrontierClaim struct {
	ClaimID    string  `json:"claimId"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	Distance   float64 `json:"distance"` // semantic distance to the question
	Hops       int64   `json:"hops"`
}

// Timing separates graph work from model work. The split is the point: it
// demonstrates that the reasoning is milliseconds of database work and the
// seconds are all language model latency.
type Timing struct {
	EmbedMS    int64 `json:"embedMs"`
	AnchorMS   int64 `json:"anchorMs"`
	SearchMS   int64 `json:"searchMs"`
	GraphMS    int64 `json:"graphMs"`
	TotalMS    int64 `json:"totalMs"`
	GraphCalls int   `json:"graphCalls"`
}

// Verdict is the result of a proof search.
type Verdict struct {
	Question string          `json:"question"`
	Status   Status          `json:"status"`
	AsOf     time.Time       `json:"asOf"`
	Chains   []Chain         `json:"chains"`
	Frontier []FrontierClaim `json:"frontier,omitempty"`
	// Reason explains an abstention in one sentence.
	Reason string `json:"reason,omitempty"`
	Timing Timing `json:"timing"`
}

// Best returns the highest-confidence chain, or nil when there is none.
func (v *Verdict) Best() *Chain {
	if len(v.Chains) == 0 {
		return nil
	}
	return &v.Chains[0]
}

// Engine runs proof searches.
type Engine struct {
	graph    kg.Reader
	embedder llm.Embedder
	// Concurrency bounds parallel graph queries during the reach phase.
	// FalkorDB serves reads in parallel from a thread pool, so fanning out here
	// is a real speedup rather than queueing - but it must stay under the
	// server's THREAD_COUNT to avoid pushing work into the queue.
	Concurrency int
}

// NewEngine builds a proof engine.
func NewEngine(graph kg.Reader, embedder llm.Embedder) *Engine {
	return &Engine{graph: graph, embedder: embedder, Concurrency: 8}
}

// Answer runs the full proof search and returns a verdict.
//
// The pipeline is:
//
//  1. embed the question
//  2. anchor it to entities (full-text and vector, unioned)
//  3. generate candidate answer claims, scoped by traversal then ranked by cosine
//  4. find grounded roots to derive from
//  5. phase one - SSpaths from each root, in parallel, to see what is reachable
//  6. phase two - SPpaths to reconstruct the best chains in full
//  7. abstain, with a frontier, if nothing clears the confidence floor
func (e *Engine) Answer(ctx context.Context, q Question) (*Verdict, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, errors.New("prove: empty question")
	}
	t0 := time.Now()
	qvec, err := e.embedder.Embed(ctx, []string{q.Text})
	if err != nil {
		return nil, fmt.Errorf("prove: embedding question: %w", err)
	}
	embedMS := ms(t0)

	v, err := e.AnswerWithVector(ctx, q, qvec[0])
	if v != nil {
		v.Timing.EmbedMS = embedMS
		v.Timing.TotalMS += embedMS
	}
	return v, err
}

// AnswerWithVector runs a proof search against an already-embedded question.
//
// The counterfactual engine re-derives the same question dozens of times, once
// per candidate retraction. Re-embedding it each time would add a network
// round trip per fork and dominate the measurement - and, worse, would make
// Feature B look like an LLM cost when it is pure graph computation. Reusing
// the vector keeps the claim "no model call" literally true.
func (e *Engine) AnswerWithVector(ctx context.Context, q Question, qvec []float64) (*Verdict, error) {
	q = q.Defaults()
	if strings.TrimSpace(q.Text) == "" {
		return nil, errors.New("prove: empty question")
	}
	if len(qvec) == 0 {
		return nil, errors.New("prove: empty question vector")
	}
	start := time.Now()
	v := &Verdict{Question: q.Text, AsOf: q.AsOf}
	var calls int
	vec := kg.VecParam(qvec)

	// 2. Anchor.
	t0 := time.Now()
	anchors, n, err := e.anchor(ctx, q, vec)
	calls += n
	if err != nil {
		return nil, err
	}
	v.Timing.AnchorMS = ms(t0)
	if len(anchors) == 0 {
		v.Status = StatusInsufficient
		v.Reason = "no entity in the graph matches this question"
		v.Timing.TotalMS = ms(start)
		v.Timing.GraphCalls = calls
		return v, nil
	}

	// 3-6. Search.
	t0 = time.Now()
	chains, frontier, n, err := e.search(ctx, q, anchors, vec)
	calls += n
	if err != nil {
		return nil, err
	}
	v.Timing.SearchMS = ms(t0)
	v.Timing.GraphMS = v.Timing.AnchorMS + v.Timing.SearchMS
	v.Timing.GraphCalls = calls
	v.Timing.TotalMS = ms(start)

	// 7. Decide.
	if len(chains) == 0 {
		v.Status = StatusInsufficient
		v.Reason = fmt.Sprintf(
			"no chain of evidence reaches an answer within %d inferential leaps",
			q.LeapBudget)
		v.Frontier = frontier
		return v, nil
	}
	if chains[0].Confidence < q.MinConfidence {
		v.Status = StatusInsufficient
		v.Reason = fmt.Sprintf(
			"the strongest chain is only %.0f%% likely, below the %.0f%% floor",
			chains[0].Confidence*100, q.MinConfidence*100)
		v.Frontier = frontier
		// The rejected chain is still returned, so a user can see what was found
		// and decide to loosen the floor rather than being told nothing.
		v.Chains = chains
		return v, nil
	}
	v.Status = StatusProven
	v.Chains = chains
	return v, nil
}

// Anchor is an entity the question resolved to.
type Anchor struct {
	ID    string
	Name  string
	Type  string
	Score float64
	// Via records which retrieval path found it, for the UI and for debugging
	// recall problems.
	Via string
}

// anchor resolves the question to entities using both retrieval paths.
//
// Full-text catches exact names, tickers and identifiers that embeddings blur
// together; the vector index catches descriptions that never name the entity.
// Neither subsumes the other, so both run and the results are unioned.
func (e *Engine) anchor(ctx context.Context, q Question, vec []any) ([]Anchor, int, error) {
	seen := map[string]*Anchor{}
	calls := 0

	ftRows, err := e.graph.Read(ctx, kg.AnchorEntityFulltext, kg.Params{
		"q":     sanitiseFulltext(q.Text),
		"limit": q.MaxRoots,
	})
	calls++
	if err != nil {
		return nil, calls, fmt.Errorf("prove: fulltext anchor: %w", err)
	}
	for _, r := range ftRows {
		id, ok := r.Str("id")
		if !ok {
			continue
		}
		seen[id] = &Anchor{
			ID: id, Name: r.StrOr("name", ""), Type: r.StrOr("type", ""),
			Score: r.FloatOr("score", 0), Via: "fulltext",
		}
	}

	vRows, err := e.graph.Read(ctx, kg.AnchorEntityVector, kg.Params{
		"k":    q.MaxRoots,
		"qvec": vec,
	})
	calls++
	if err != nil {
		return nil, calls, fmt.Errorf("prove: vector anchor: %w", err)
	}
	for _, r := range vRows {
		id, ok := r.Str("id")
		if !ok {
			continue
		}
		if a, dup := seen[id]; dup {
			a.Via = "fulltext+vector" // found by both: a strong anchor
			continue
		}
		seen[id] = &Anchor{
			ID: id, Name: r.StrOr("name", ""), Type: r.StrOr("type", ""),
			Score: r.FloatOr("score", 0), Via: "vector",
		}
	}

	out := make([]Anchor, 0, len(seen))
	for _, a := range seen {
		out = append(out, *a)
	}
	// Anchors found by both paths sort first: agreement between an exact-match
	// index and a semantic one is the strongest signal available here.
	sort.Slice(out, func(i, j int) bool {
		bi, bj := out[i].Via == "fulltext+vector", out[j].Via == "fulltext+vector"
		if bi != bj {
			return bi
		}
		return out[i].Score > out[j].Score
	})
	return out, calls, nil
}

type reachTip struct {
	weight int64
	cost   int64
	hops   int64
	rootID string
}

// search runs the two-phase proof discovery.
func (e *Engine) search(ctx context.Context, q Question, anchors []Anchor, vec []any) ([]Chain, []FrontierClaim, int, error) {
	calls := 0
	anchorIDs := make([]any, 0, len(anchors))
	for _, a := range anchors {
		anchorIDs = append(anchorIDs, a.ID)
	}
	asOf := q.AsOf.UnixMilli()

	// 3. Candidate answers: traversal-scoped, then semantically ranked.
	candRows, err := e.graph.Read(ctx, kg.CandidateClaimsScoped, kg.Params{
		"anchorIDs": anchorIDs,
		"qvec":      vec,
		"asOf":      asOf,
		"maxDist":   q.MaxSemanticDistance,
		"limit":     q.MaxCandidates,
	})
	calls++
	if err != nil {
		return nil, nil, calls, fmt.Errorf("prove: candidate generation: %w", err)
	}
	candidates := map[string]kg.Row{}
	for _, r := range candRows {
		if id, ok := r.Str("id"); ok {
			candidates[id] = r
		}
	}
	if len(candidates) == 0 {
		return nil, nil, calls, nil
	}

	// 4. Grounded roots.
	rootRows, err := e.graph.Read(ctx, kg.ProveRoots, kg.Params{
		"anchorIDs": anchorIDs,
		"asOf":      asOf,
		"minConf":   q.MinConfidence,
		"limit":     q.MaxRoots,
	})
	calls++
	if err != nil {
		return nil, nil, calls, fmt.Errorf("prove: root discovery: %w", err)
	}
	roots := make([]string, 0, len(rootRows))
	rootConf := make(map[string]float64, len(rootRows))
	for _, r := range rootRows {
		if id, ok := r.Str("id"); ok {
			roots = append(roots, id)
			rootConf[id] = r.FloatOr("conf", 1)
		}
	}
	if len(roots) == 0 {
		return nil, nil, calls, nil
	}

	// 5. Phase one: one SSpaths per root, in parallel.
	best := map[string]reachTip{} // candidate claim id -> cheapest reach
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, e.Concurrency))
	errs := make([]error, len(roots))

	for i, rootID := range roots {
		wg.Add(1)
		go func(i int, rootID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			rows, err := e.graph.Read(ctx, kg.ProveReach, kg.Params{
				"srcID":      rootID,
				"leapBudget": q.LeapBudget,
				"maxHops":    q.MaxHops,
			})
			if err != nil {
				errs[i] = err
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, r := range rows {
				id, ok := r.Str("id")
				if !ok {
					continue
				}
				if _, wanted := candidates[id]; !wanted {
					continue
				}
				// The premise's own confidence is not on any ENTAILS edge, so it
				// must be folded into the path weight here or the chain's
				// probability would silently omit it.
				w := r.IntOr("weight", 0) + kg.WeightFromConfidence(rootConf[rootID])
				if cur, seen := best[id]; seen && cur.weight <= w {
					continue
				}
				best[id] = reachTip{
					weight: w,
					cost:   r.IntOr("cost", 0),
					hops:   r.IntOr("hops", 0),
					rootID: rootID,
				}
			}
		}(i, rootID)
	}
	wg.Wait()
	calls += len(roots)

	for _, err := range errs {
		if err != nil {
			return nil, nil, calls, fmt.Errorf("prove: reach phase: %w", err)
		}
	}

	// No candidate is derivable inside the budget: abstain, and explain where
	// the evidence stopped.
	if len(best) == 0 {
		frontier, n, ferr := e.frontier(ctx, q, roots, vec)
		calls += n
		if ferr != nil {
			// A failed frontier query must not turn a legitimate abstention into
			// an error - the abstention is still the correct answer.
			return nil, nil, calls, nil
		}
		return nil, frontier, calls, nil
	}

	// 6. Phase two: reconstruct the best chains in full.
	type scored struct {
		claimID string
		tip     reachTip
	}
	ranked := make([]scored, 0, len(best))
	for id, t := range best {
		ranked = append(ranked, scored{id, t})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].tip.weight < ranked[j].tip.weight })
	if len(ranked) > 3 {
		ranked = ranked[:3]
	}

	var chains []Chain
	for _, s := range ranked {
		rows, err := e.graph.Read(ctx, kg.ProveChain, kg.Params{
			"srcID":      s.tip.rootID,
			"dstID":      s.claimID,
			"leapBudget": q.LeapBudget,
			"maxHops":    q.MaxHops,
			"pathCount":  int64(3),
		})
		calls++
		if err != nil {
			return nil, nil, calls, fmt.Errorf("prove: chain reconstruction: %w", err)
		}
		for _, r := range rows {
			c, ok := chainFromRow(r, rootConf[s.tip.rootID])
			if ok {
				chains = append(chains, c)
			}
		}
	}
	sort.Slice(chains, func(i, j int) bool { return chains[i].Weight < chains[j].Weight })
	return chains, nil, calls, nil
}

// frontier finds where the evidence runs out, so an abstention is actionable.
func (e *Engine) frontier(ctx context.Context, q Question, roots []string, vec []any) ([]FrontierClaim, int, error) {
	calls := 0
	var out []FrontierClaim
	// One root is enough to show the shape of the gap; querying all of them
	// would multiply latency on a path the user is already not getting an
	// answer from.
	for _, rootID := range roots[:min(2, len(roots))] {
		rows, err := e.graph.Read(ctx, kg.ProveFrontier, kg.Params{
			"srcID":      rootID,
			"leapBudget": q.LeapBudget,
			"maxHops":    q.MaxHops,
			"qvec":       vec,
			"limit":      int64(5),
		})
		calls++
		if err != nil {
			return nil, calls, err
		}
		for _, r := range rows {
			id, ok := r.Str("id")
			if !ok {
				continue
			}
			out = append(out, FrontierClaim{
				ClaimID:    id,
				Text:       r.StrOr("text", ""),
				Confidence: r.FloatOr("conf", 0),
				Distance:   r.FloatOr("dist", 1),
				Hops:       r.IntOr("hops", 0),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Distance < out[j].Distance })
	if len(out) > 5 {
		out = out[:5]
	}
	return out, calls, nil
}

// chainFromRow decodes one ProveChain result. rootConf is the grounded root
// claim's own confidence, which carries no ENTAILS edge and so must be folded
// in separately from the edge weights the query returns.
func chainFromRow(r kg.Row, rootConf float64) (Chain, bool) {
	ids, ok := r.Strings("claimIDs")
	if !ok || len(ids) == 0 {
		return Chain{}, false
	}
	texts, _ := r.Strings("claimTexts")
	weights, _ := r.Ints("stepWeights")

	total := r.IntOr("weight", 0) + kg.WeightFromConfidence(rootConf)
	c := Chain{
		Weight:     total,
		Confidence: kg.ConfidenceFromWeight(total),
		Leaps:      r.IntOr("cost", 0),
		Hops:       r.IntOr("hops", int64(len(ids)-1)),
		Steps:      make([]Step, 0, len(ids)),
	}
	for i, id := range ids {
		s := Step{ClaimID: id}
		if i < len(texts) {
			s.Text = texts[i]
		}
		// stepWeights has one entry per edge, so it is one shorter than the node
		// list. The first node is the root premise and is reached by no edge.
		if i > 0 && i-1 < len(weights) {
			s.Weight = weights[i-1]
			s.Conf = kg.ConfidenceFromWeight(weights[i-1])
		} else {
			s.Conf = rootConf
		}
		c.Steps = append(c.Steps, s)
	}
	return c, true
}

// sanitiseFulltext strips characters that are operators in the RediSearch query
// syntax behind FalkorDB's full-text index. Without this a question containing
// a hyphen or a bracket becomes a malformed query rather than a search for
// those words. The value is still passed as a bound parameter; this only
// prevents a syntax error, it is not the injection defence.
func sanitiseFulltext(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '@', '!', '{', '}', '(', ')', '[', ']', '|', '-', '~', '*', '"', ':', '\'', '\\', '/', '>', '<', '=':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func ms(t time.Time) int64 { return time.Since(t).Milliseconds() }
