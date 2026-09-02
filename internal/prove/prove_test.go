package prove

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/prince/argus/internal/kg"
)

// fakeGraph returns canned rows per template name, so the proof engine's
// decision logic can be tested without a database. Decoding results into
// kg.Rows at the client boundary is what makes this possible.
type fakeGraph struct {
	mu    sync.Mutex
	rows  map[string]kg.Rows
	errs  map[string]error
	calls map[string]int
	// perSrc lets prove.reach return different results per root claim.
	perSrc map[string]kg.Rows
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{
		rows:   map[string]kg.Rows{},
		errs:   map[string]error{},
		calls:  map[string]int{},
		perSrc: map[string]kg.Rows{},
	}
}

func (f *fakeGraph) Read(ctx context.Context, t kg.Template, p kg.Params) (kg.Rows, error) {
	return f.ReadGraph(ctx, "argus", t, p)
}

func (f *fakeGraph) ReadGraph(_ context.Context, _ string, t kg.Template, p kg.Params) (kg.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[t.Name]++
	if err := f.errs[t.Name]; err != nil {
		return nil, err
	}
	if t.Name == kg.ProveReach.Name {
		src, _ := p["srcID"].(string)
		if r, ok := f.perSrc[src]; ok {
			return r, nil
		}
	}
	return f.rows[t.Name], nil
}

func (f *fakeGraph) Plan(context.Context, kg.Template, kg.Params) (string, error) {
	return "", nil
}
func (f *fakeGraph) GraphName() string { return "argus" }
func (f *fakeGraph) Close() error      { return nil }

func (f *fakeGraph) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

// fakeEmbedder returns a fixed vector; the proof engine never inspects it.
type fakeEmbedder struct{ err error }

func (f fakeEmbedder) Embed(_ context.Context, in []string) ([][]float64, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float64, len(in))
	for i := range in {
		out[i] = []float64{0.1, 0.2, 0.3}
	}
	return out, nil
}
func (f fakeEmbedder) Dimensions() int { return 3 }
func (f fakeEmbedder) Model() string   { return "fake" }

// wireHappyPath sets up a graph where root claim r1 derives candidate c9
// through two inference steps.
func wireHappyPath(g *fakeGraph) {
	g.rows[kg.AnchorEntityFulltext.Name] = kg.Rows{
		{"id": "e:acme", "name": "Acme Corp", "type": "Company", "score": 4.2},
	}
	g.rows[kg.AnchorEntityVector.Name] = kg.Rows{
		{"id": "e:acme", "name": "Acme Corp", "type": "Company", "score": 0.9},
		{"id": "e:zeta", "name": "Zeta Holdings", "type": "Company", "score": 0.7},
	}
	g.rows[kg.CandidateClaimsScoped.Name] = kg.Rows{
		{"id": "c9", "text": "Zeta controlled Acme", "conf": 0.8, "hops": int64(1), "dist": 0.21},
	}
	g.rows[kg.ProveRoots.Name] = kg.Rows{
		{"id": "r1", "text": "Filing lists Zeta as 60% holder of Acme", "conf": 1.0},
	}
	// 223 + 470 = 693 milli-nats  ->  exp(-0.693) ~= 0.50
	g.perSrc["r1"] = kg.Rows{
		{"id": "c9", "weight": int64(693), "cost": int64(2), "hops": int64(2)},
	}
	g.rows[kg.ProveChain.Name] = kg.Rows{
		{
			"weight":      int64(693),
			"cost":        int64(2),
			"hops":        int64(2),
			"claimIDs":    []any{"r1", "m5", "c9"},
			"claimTexts":  []any{"Filing lists Zeta as 60% holder", "60% is a controlling stake", "Zeta controlled Acme"},
			"stepWeights": []any{int64(223), int64(470)},
		},
	}
}

func TestAnswer_ProvenChain(t *testing.T) {
	g := newFakeGraph()
	wireHappyPath(g)
	e := NewEngine(g, fakeEmbedder{})

	v, err := e.Answer(context.Background(), Question{Text: "Did Zeta control Acme?"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if v.Status != StatusProven {
		t.Fatalf("status = %q, want %q (reason: %s)", v.Status, StatusProven, v.Reason)
	}
	best := v.Best()
	if best == nil {
		t.Fatal("expected a chain")
	}
	if len(best.Steps) != 3 {
		t.Fatalf("chain has %d steps, want 3", len(best.Steps))
	}
	// exp(-693/1000) = 0.5001...
	if best.Confidence < 0.49 || best.Confidence > 0.51 {
		t.Errorf("confidence = %.4f, want ~0.50 (recovered from pathWeight 693)", best.Confidence)
	}
	if best.Leaps != 2 {
		t.Errorf("leaps = %d, want 2", best.Leaps)
	}
	// The root premise is reached by no edge, so it must be certain rather than
	// inheriting a step weight.
	if best.Steps[0].Conf != 1 {
		t.Errorf("root step confidence = %v, want 1", best.Steps[0].Conf)
	}
	if best.Steps[1].Weight != 223 || best.Steps[2].Weight != 470 {
		t.Errorf("step weights misaligned: got %d, %d; want 223, 470 "+
			"(stepWeights has one entry per edge, one fewer than nodes)",
			best.Steps[1].Weight, best.Steps[2].Weight)
	}
}

// The defining behaviour: when no derivation exists inside the budget, ARGUS
// must abstain rather than answer. A vector store cannot do this because top-k
// always returns something.
func TestAnswer_AbstainsWhenNoChainExists(t *testing.T) {
	g := newFakeGraph()
	wireHappyPath(g)
	g.perSrc["r1"] = kg.Rows{} // nothing reachable inside the budget
	g.rows[kg.ProveFrontier.Name] = kg.Rows{
		{"id": "f1", "text": "Zeta filed a Form 4 in 2019", "conf": 0.9, "dist": 0.4, "hops": int64(1)},
	}
	e := NewEngine(g, fakeEmbedder{})

	v, err := e.Answer(context.Background(), Question{Text: "Did Zeta control Acme?"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if v.Status != StatusInsufficient {
		t.Fatalf("status = %q, want %q", v.Status, StatusInsufficient)
	}
	if len(v.Chains) != 0 {
		t.Errorf("expected no chains, got %d", len(v.Chains))
	}
	// An abstention must be actionable: show where the evidence stopped.
	if len(v.Frontier) == 0 {
		t.Error("abstention returned no frontier; a refusal must point at the gap")
	}
	if !strings.Contains(v.Reason, "inferential leaps") {
		t.Errorf("reason %q should explain the budget that was exhausted", v.Reason)
	}
}

func TestAnswer_AbstainsBelowConfidenceFloor(t *testing.T) {
	g := newFakeGraph()
	wireHappyPath(g)
	// 2996 milli-nats -> exp(-2.996) ~= 0.05, well under the floor.
	g.perSrc["r1"] = kg.Rows{
		{"id": "c9", "weight": int64(2996), "cost": int64(2), "hops": int64(2)},
	}
	g.rows[kg.ProveChain.Name] = kg.Rows{
		{
			"weight": int64(2996), "cost": int64(2), "hops": int64(2),
			"claimIDs":    []any{"r1", "c9"},
			"claimTexts":  []any{"root", "answer"},
			"stepWeights": []any{int64(2996)},
		},
	}
	e := NewEngine(g, fakeEmbedder{})

	v, err := e.Answer(context.Background(), Question{Text: "Did Zeta control Acme?", MinConfidence: 0.35})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if v.Status != StatusInsufficient {
		t.Fatalf("status = %q, want %q", v.Status, StatusInsufficient)
	}
	// The weak chain is still returned so the user can loosen the floor
	// deliberately rather than being told nothing was found.
	if len(v.Chains) == 0 {
		t.Error("a below-floor chain should still be surfaced, not hidden")
	}
	if !strings.Contains(v.Reason, "%") {
		t.Errorf("reason %q should quantify the shortfall", v.Reason)
	}
}

func TestAnswer_AbstainsWhenNothingAnchors(t *testing.T) {
	g := newFakeGraph() // every template returns nil
	e := NewEngine(g, fakeEmbedder{})

	v, err := e.Answer(context.Background(), Question{Text: "Who owns Nonexistent Ltd?"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if v.Status != StatusInsufficient {
		t.Fatalf("status = %q, want %q", v.Status, StatusInsufficient)
	}
	if !strings.Contains(v.Reason, "no entity") {
		t.Errorf("reason %q should say the question did not anchor", v.Reason)
	}
	// Searching is pointless without an anchor and must be skipped.
	if n := g.callCount(kg.CandidateClaimsScoped.Name); n != 0 {
		t.Errorf("candidate generation ran %d times despite no anchors", n)
	}
}

func TestAnchor_UnionsBothPathsAndPrefersAgreement(t *testing.T) {
	g := newFakeGraph()
	wireHappyPath(g)
	e := NewEngine(g, fakeEmbedder{})

	anchors, calls, err := e.anchor(context.Background(), Question{}.Defaults(), []any{0.1})
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if calls != 2 {
		t.Errorf("anchor made %d graph calls, want 2 (fulltext + vector)", calls)
	}
	if len(anchors) != 2 {
		t.Fatalf("got %d anchors, want 2 deduplicated", len(anchors))
	}
	// e:acme was found by both paths, which is the strongest signal, so it must
	// sort first even though its vector score is compared against a different scale.
	if anchors[0].ID != "e:acme" {
		t.Errorf("first anchor = %q, want e:acme (found by both paths)", anchors[0].ID)
	}
	if anchors[0].Via != "fulltext+vector" {
		t.Errorf("Via = %q, want fulltext+vector", anchors[0].Via)
	}
}

// Reach runs one query per root, concurrently. Verifying the call count guards
// the two-phase design against regressing into an O(roots x candidates) search.
func TestSearch_IsOneReachQueryPerRoot(t *testing.T) {
	g := newFakeGraph()
	wireHappyPath(g)
	g.rows[kg.ProveRoots.Name] = kg.Rows{
		{"id": "r1", "text": "a", "conf": 1.0},
		{"id": "r2", "text": "b", "conf": 0.9},
		{"id": "r3", "text": "c", "conf": 0.9},
	}
	e := NewEngine(g, fakeEmbedder{})

	if _, err := e.Answer(context.Background(), Question{Text: "q"}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if n := g.callCount(kg.ProveReach.Name); n != 3 {
		t.Errorf("reach ran %d times for 3 roots, want 3", n)
	}
}

// A failure in the frontier query must not convert a correct abstention into an
// error - the abstention is still the right answer.
func TestAnswer_FrontierFailureStillAbstains(t *testing.T) {
	g := newFakeGraph()
	wireHappyPath(g)
	g.perSrc["r1"] = kg.Rows{}
	g.errs[kg.ProveFrontier.Name] = errors.New("boom")
	e := NewEngine(g, fakeEmbedder{})

	v, err := e.Answer(context.Background(), Question{Text: "q"})
	if err != nil {
		t.Fatalf("frontier failure should not fail the request: %v", err)
	}
	if v.Status != StatusInsufficient {
		t.Fatalf("status = %q, want %q", v.Status, StatusInsufficient)
	}
}

func TestAnswer_PropagatesGraphErrors(t *testing.T) {
	g := newFakeGraph()
	wireHappyPath(g)
	g.errs[kg.CandidateClaimsScoped.Name] = errors.New("connection reset")
	e := NewEngine(g, fakeEmbedder{})

	if _, err := e.Answer(context.Background(), Question{Text: "q"}); err == nil {
		t.Fatal("a graph failure must surface as an error, not a silent abstention")
	}
}

func TestAnswer_RejectsEmptyQuestion(t *testing.T) {
	e := NewEngine(newFakeGraph(), fakeEmbedder{})
	if _, err := e.Answer(context.Background(), Question{Text: "   "}); err == nil {
		t.Fatal("expected an error for a blank question")
	}
}

func TestTimingSeparatesGraphFromModel(t *testing.T) {
	g := newFakeGraph()
	wireHappyPath(g)
	e := NewEngine(g, fakeEmbedder{})

	v, err := e.Answer(context.Background(), Question{Text: "q"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if v.Timing.GraphCalls < 4 {
		t.Errorf("GraphCalls = %d, want at least 4 (2 anchors + candidates + roots)", v.Timing.GraphCalls)
	}
	if v.Timing.GraphMS != v.Timing.AnchorMS+v.Timing.SearchMS {
		t.Error("GraphMS must be the sum of the graph phases so the HUD can trust it")
	}
}

func TestSanitiseFulltext(t *testing.T) {
	cases := map[string]string{
		"Did Zeta-Holdings control Acme?": "Did Zeta Holdings control Acme?",
		"who owns @acme (2019)":           "who owns acme 2019",
		`a "quoted" | phrase`:             "a quoted phrase",
		"  spaced   out  ":                "spaced out",
	}
	for in, want := range cases {
		if got := sanitiseFulltext(in); got != want {
			t.Errorf("sanitiseFulltext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuestionDefaults(t *testing.T) {
	q := Question{Text: "x"}.Defaults()
	if q.LeapBudget == 0 || q.MaxHops == 0 || q.MinConfidence == 0 {
		t.Fatal("Defaults left a search bound unset, which would make the query unbounded")
	}
	if q.AsOf.IsZero() {
		t.Fatal("AsOf must default to now so the temporal filter is always applied")
	}
	// An explicit value must survive.
	q2 := Question{Text: "x", LeapBudget: 2}.Defaults()
	if q2.LeapBudget != 2 {
		t.Errorf("Defaults overwrote an explicit LeapBudget: got %d", q2.LeapBudget)
	}
}
