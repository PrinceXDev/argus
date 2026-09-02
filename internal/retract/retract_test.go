package retract

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/prove"
)

// forkGraph is a fake FalkorDB that models the one behaviour Feature B depends
// on: GRAPH.COPY produces an independent graph, and ablating a claim in a fork
// leaves the source untouched.
type forkGraph struct {
	mu sync.Mutex

	// reach maps a graph key to the claims still derivable in it, with the
	// weight of the best path reaching each.
	reach map[string]map[string]int64
	// retracted records which claim was ablated in each fork, for assertions.
	retracted map[string]string
	// copies counts GRAPH.COPY calls.
	copies int
	// live tracks fork keys currently in existence, so leaks are detectable.
	live map[string]bool
	// maxLive is the high-water mark, for the concurrency bound.
	maxLive int

	copyErr error
}

const (
	primary = "argus"
	rootID  = "r1"
	answer  = "c9"
)

func newForkGraph() *forkGraph {
	return &forkGraph{
		reach: map[string]map[string]int64{
			// Base world: r1 -> m5 -> c9 at 223 milli-nats (~0.80). The base is
			// deliberately well above the 0.35 abstention floor, so a weakened
			// derivation has room to be "major" without tipping into abstention.
			primary: {answer: 223},
		},
		retracted: map[string]string{},
		live:      map[string]bool{},
	}
}

func (f *forkGraph) GraphName() string { return primary }
func (f *forkGraph) Close() error      { return nil }

func (f *forkGraph) CopyGraph(_ context.Context, src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.copyErr != nil {
		return f.copyErr
	}
	if !kg.IsForkName(dst) {
		return kg.ErrNotAFork
	}
	src2 := map[string]int64{}
	for k, v := range f.reach[src] {
		src2[k] = v
	}
	f.reach[dst] = src2
	f.copies++
	f.live[dst] = true
	if len(f.live) > f.maxLive {
		f.maxLive = len(f.live)
	}
	return nil
}

func (f *forkGraph) DropGraph(_ context.Context, graph string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if graph == primary {
		return kg.ErrPrimaryGraph
	}
	delete(f.reach, graph)
	delete(f.live, graph)
	return nil
}

func (f *forkGraph) ListGraphs(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.reach {
		out = append(out, k)
	}
	return out, nil
}

// WriteGraph models retraction: ablating a claim removes whatever it made
// derivable, per the wiring in ablate.
func (f *forkGraph) WriteGraph(_ context.Context, graph string, t kg.Template, p kg.Params) (kg.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.Name != kg.RetractClaim.Name {
		return nil, nil
	}
	id, _ := p["claimID"].(string)
	f.retracted[graph] = id
	if eff, ok := ablate[id]; ok {
		if eff.collapse {
			delete(f.reach[graph], answer)
		} else {
			f.reach[graph][answer] = eff.weight
		}
	}
	return nil, nil
}

// ablate declares what removing each claim does to the derivation.
var ablate = map[string]struct {
	collapse bool
	weight   int64
}{
	// The load-bearing fact: without it nothing reaches the answer.
	"m5": {collapse: true},
	// A corroborated step: an alternative route exists, weaker but still above
	// the abstention floor (exp(-0.900) ~= 0.41 > 0.35).
	"r1": {weight: 900},
	// A claim the derivation does not depend on at all.
	answer: {weight: 223},
}

func (f *forkGraph) ReadGraph(_ context.Context, graph string, t kg.Template, p kg.Params) (kg.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch t.Name {
	case kg.AnchorEntityFulltext.Name:
		return kg.Rows{{"id": "e:acme", "name": "Acme", "type": "Company", "score": 1.0}}, nil
	case kg.AnchorEntityVector.Name:
		return kg.Rows{{"id": "e:acme", "name": "Acme", "type": "Company", "score": 0.9}}, nil
	case kg.CandidateClaimsScoped.Name:
		return kg.Rows{{"id": answer, "text": "Zeta controlled Acme", "conf": 0.8,
			"hops": int64(1), "dist": 0.2}}, nil
	case kg.ProveRoots.Name:
		return kg.Rows{{"id": rootID, "text": "root premise", "conf": 1.0}}, nil
	case kg.ProveReach.Name:
		w, ok := f.reach[graph][answer]
		if !ok {
			return nil, nil
		}
		return kg.Rows{{"id": answer, "weight": w, "cost": int64(2), "hops": int64(2)}}, nil
	case kg.ProveChain.Name:
		w, ok := f.reach[graph][answer]
		if !ok {
			return nil, nil
		}
		return kg.Rows{{
			"weight": w, "cost": int64(2), "hops": int64(2),
			"claimIDs":    []any{rootID, "m5", answer},
			"claimTexts":  []any{"root premise", "60% is controlling", "Zeta controlled Acme"},
			"stepWeights": []any{int64(223), w - 223},
		}}, nil
	case kg.ProveFrontier.Name:
		return kg.Rows{{"id": "f1", "text": "frontier", "conf": 0.9,
			"dist": 0.5, "hops": int64(1)}}, nil
	}
	return nil, nil
}

func (f *forkGraph) Read(ctx context.Context, t kg.Template, p kg.Params) (kg.Rows, error) {
	return f.ReadGraph(ctx, primary, t, p)
}
func (f *forkGraph) Write(ctx context.Context, t kg.Template, p kg.Params) (kg.Rows, error) {
	return f.WriteGraph(ctx, primary, t, p)
}
func (f *forkGraph) Plan(context.Context, kg.Template, kg.Params) (string, error) { return "", nil }
func (f *forkGraph) Raw(context.Context, string, string) (kg.Rows, error)         { return nil, nil }

// ─── helpers ─────────────────────────────────────────────────────────────────

func baseVerdict() *prove.Verdict {
	return &prove.Verdict{
		Question: "Did Zeta control Acme?",
		Status:   prove.StatusProven,
		Chains: []prove.Chain{{
			Confidence: kg.ConfidenceFromWeight(223),
			Weight:     223,
			Leaps:      2,
			Hops:       2,
			Steps: []prove.Step{
				{ClaimID: rootID, Text: "root premise", Conf: 1},
				{ClaimID: "m5", Text: "60% is controlling", Weight: 111},
				{ClaimID: answer, Text: "Zeta controlled Acme", Weight: 112},
			},
		}},
	}
}

func newEngine(g *forkGraph) *Engine {
	// The prover is constructed with a nil embedder on purpose: every path
	// through Feature B must use AnswerWithVector, so touching the embedder
	// would panic and fail the test loudly.
	return NewEngine(g, prove.NewEngine(g, nil))
}

var qvec = []float64{0.1, 0.2, 0.3}

// ─── tests ───────────────────────────────────────────────────────────────────

// The headline capability: identify the fact the conclusion actually rests on.
func TestAnalyse_IdentifiesTheLoadBearingFact(t *testing.T) {
	g := newForkGraph()
	e := newEngine(g)

	a, err := e.Analyse(context.Background(),
		prove.Question{Text: "Did Zeta control Acme?"}, qvec, baseVerdict())
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	if len(a.LoadBearing) != 1 || a.LoadBearing[0] != "m5" {
		t.Fatalf("LoadBearing = %v, want [m5]", a.LoadBearing)
	}
	// Most impactful first, so the headline finding is the first row the UI
	// renders.
	if a.Retractions[0].ClaimID != "m5" {
		t.Errorf("first retraction = %s, want m5", a.Retractions[0].ClaimID)
	}
	if a.Retractions[0].Impact != ImpactCritical {
		t.Errorf("impact = %s, want %s", a.Retractions[0].Impact, ImpactCritical)
	}
	if !a.Retractions[0].Collapses {
		t.Error("retracting the load-bearing fact must collapse the answer")
	}
}

// A claim with an alternative route is corroborated, not load-bearing - and the
// distinction is the whole analytical value of the feature.
func TestAnalyse_DistinguishesCorroboratedFromLoadBearing(t *testing.T) {
	g := newForkGraph()
	e := newEngine(g)

	a, err := e.Analyse(context.Background(),
		prove.Question{Text: "q"}, qvec, baseVerdict())
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	byID := map[string]Retraction{}
	for _, r := range a.Retractions {
		byID[r.ClaimID] = r
	}

	weakened := byID[rootID]
	if weakened.Collapses {
		t.Error("r1 has an alternative route above the abstention floor and must not " +
			"be reported as load-bearing")
	}
	// 0.80 -> 0.41 is a ~49% relative drop that still clears the 0.35 floor:
	// major, not critical.
	if weakened.Impact != ImpactMajor {
		t.Errorf("r1 impact = %s, want %s (delta %.3f of base %.3f)",
			weakened.Impact, ImpactMajor, weakened.Delta, weakened.BaseConfidence)
	}
	if weakened.AlternativeHops == 0 {
		t.Error("a surviving derivation should report its length")
	}

	unaffected := byID[answer]
	if unaffected.Impact != ImpactNone {
		t.Errorf("a claim with no effect should be %s, got %s", ImpactNone, unaffected.Impact)
	}
}

// The claim that makes the feature defensible: this is graph computation, not
// model inference.
func TestAnalyse_MakesNoModelCalls(t *testing.T) {
	g := newForkGraph()
	// A nil embedder means any attempt to embed panics. Reaching the end proves
	// nothing tried.
	e := newEngine(g)

	a, err := e.Analyse(context.Background(), prove.Question{Text: "q"}, qvec, baseVerdict())
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	if a.LLMCalls != 0 {
		t.Errorf("LLMCalls = %d, want 0", a.LLMCalls)
	}
}

// Each candidate needs its own world, or the ablations contaminate each other.
func TestAnalyse_ForksOncePerCandidate(t *testing.T) {
	g := newForkGraph()
	e := newEngine(g)

	a, err := e.Analyse(context.Background(), prove.Question{Text: "q"}, qvec, baseVerdict())
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	if g.copies != 3 {
		t.Errorf("made %d forks for 3 candidates, want 3", g.copies)
	}
	if a.ForksUsed != 3 {
		t.Errorf("ForksUsed = %d, want 3", a.ForksUsed)
	}
}

// A fork left behind is a memory leak in a database that holds graphs in RAM.
func TestAnalyse_ReapsEveryFork(t *testing.T) {
	g := newForkGraph()
	e := newEngine(g)

	if _, err := e.Analyse(context.Background(), prove.Question{Text: "q"}, qvec, baseVerdict()); err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	graphs, _ := g.ListGraphs(context.Background())
	for _, name := range graphs {
		if kg.IsForkName(name) {
			t.Errorf("fork %q survived the sweep", name)
		}
	}
	if len(g.live) != 0 {
		t.Errorf("%d fork(s) still live", len(g.live))
	}
}

// Forks are full graph copies, so unbounded concurrency is a memory hazard.
func TestAnalyse_RespectsConcurrencyBound(t *testing.T) {
	g := newForkGraph()
	e := newEngine(g)
	e.Concurrency = 2

	if _, err := e.Analyse(context.Background(), prove.Question{Text: "q"}, qvec, baseVerdict()); err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	if g.maxLive > 2 {
		t.Errorf("%d forks existed simultaneously, want at most 2", g.maxLive)
	}
}

// The source corpus must be untouched: other queries are running against it.
func TestAnalyse_LeavesThePrimaryGraphIntact(t *testing.T) {
	g := newForkGraph()
	before := g.reach[primary][answer]
	e := newEngine(g)

	if _, err := e.Analyse(context.Background(), prove.Question{Text: "q"}, qvec, baseVerdict()); err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	if got := g.reach[primary][answer]; got != before {
		t.Errorf("the primary graph changed: weight %d -> %d", before, got)
	}
	if _, ablated := g.retracted[primary]; ablated {
		t.Error("a claim was ablated in the primary graph rather than in a fork")
	}
}

func TestAnalyse_RejectsVerdictWithNoChains(t *testing.T) {
	e := newEngine(newForkGraph())
	_, err := e.Analyse(context.Background(), prove.Question{Text: "q"}, qvec,
		&prove.Verdict{Status: prove.StatusInsufficient})
	if err == nil {
		t.Fatal("expected an error when there is nothing to analyse")
	}
}

func TestAnalyse_RequiresPrecomputedVector(t *testing.T) {
	e := newEngine(newForkGraph())
	_, err := e.Analyse(context.Background(), prove.Question{Text: "q"}, nil, baseVerdict())
	if err == nil {
		t.Fatal("expected an error without a question vector")
	}
	if !strings.Contains(err.Error(), "vector") {
		t.Errorf("error should name the missing vector: %v", err)
	}
}

func TestAnalyse_PropagatesForkFailure(t *testing.T) {
	g := newForkGraph()
	g.copyErr = kg.ErrNotAFork
	e := newEngine(g)

	if _, err := e.Analyse(context.Background(), prove.Question{Text: "q"}, qvec, baseVerdict()); err == nil {
		t.Fatal("a failed fork must surface, not be silently skipped")
	}
}

func TestReapForks(t *testing.T) {
	g := newForkGraph()
	ctx := context.Background()
	_ = g.CopyGraph(ctx, primary, kg.ForkPrefix+"orphan1")
	_ = g.CopyGraph(ctx, primary, kg.ForkPrefix+"orphan2")

	n, err := ReapForks(ctx, g)
	if err != nil {
		t.Fatalf("ReapForks: %v", err)
	}
	if n != 2 {
		t.Errorf("reaped %d forks, want 2", n)
	}
	if _, ok := g.reach[primary]; !ok {
		t.Fatal("the reaper deleted the primary graph")
	}
}

func TestShortID_ProducesSafeKeys(t *testing.T) {
	for _, in := range []string{"c:abc123", "c:" + strings.Repeat("f", 40), "weird/id:with*chars"} {
		got := shortID(in)
		for i := 0; i < len(got); i++ {
			c := got[i]
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
			if !ok {
				t.Errorf("shortID(%q) = %q contains an unsafe key character %q", in, got, string(c))
			}
		}
		if len(got) > 12 {
			t.Errorf("shortID(%q) = %q is longer than 12 chars", in, got)
		}
	}
}
