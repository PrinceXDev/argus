package court

import (
	"context"
	"strings"
	"testing"

	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/prove"
)

type fakeGraph struct {
	rows map[string]kg.Rows
	errs map[string]error
}

func newFake() *fakeGraph {
	return &fakeGraph{rows: map[string]kg.Rows{}, errs: map[string]error{}}
}

func (f *fakeGraph) Read(_ context.Context, t kg.Template, _ kg.Params) (kg.Rows, error) {
	if err := f.errs[t.Name]; err != nil {
		return nil, err
	}
	return f.rows[t.Name], nil
}
func (f *fakeGraph) ReadGraph(ctx context.Context, _ string, t kg.Template, p kg.Params) (kg.Rows, error) {
	return f.Read(ctx, t, p)
}
func (f *fakeGraph) Plan(context.Context, kg.Template, kg.Params) (string, error) { return "", nil }
func (f *fakeGraph) GraphName() string                                            { return "argus" }
func (f *fakeGraph) Close() error                                                 { return nil }

func verdict() *prove.Verdict {
	return &prove.Verdict{
		Status: prove.StatusProven,
		Chains: []prove.Chain{{
			Confidence: 0.8,
			Steps: []prove.Step{
				{ClaimID: "c1", Text: "Zeta held 60% of Acme"},
				{ClaimID: "c2", Text: "Zeta controlled Acme"},
			},
		}},
	}
}

// The defining behaviour: a dispute must be surfaced, not silently resolved.
func TestAdjudicate_SurfacesDisputes(t *testing.T) {
	f := newFake()
	f.rows[kg.CourtContradictions.Name] = kg.Rows{{
		"claimID": "c1", "counterID": "x9",
		"counterText": "Zeta held 71.2% of Acme", "counterConf": 0.6,
		"detectedBy": "nli", "sourceID": "s:wire", "sourceName": "Market Wire Daily",
		"sourceTrust": 0.4,
	}}
	e := NewEngine(f)

	r, err := e.Adjudicate(context.Background(), verdict())
	if err != nil {
		t.Fatalf("Adjudicate: %v", err)
	}
	if !r.Contested {
		t.Fatal("a chain with a contradicted claim must be marked contested")
	}
	if len(r.Disputes) != 1 {
		t.Fatalf("got %d disputes, want 1", len(r.Disputes))
	}
	d := r.Disputes[0]
	if d.CounterSource.Name != "Market Wire Daily" {
		t.Errorf("counter source = %q", d.CounterSource.Name)
	}
	// A false positive must be traceable to its detector rather than argued
	// about.
	if d.DetectedBy != "nli" {
		t.Errorf("DetectedBy = %q, want the detector that fired", d.DetectedBy)
	}
}

func TestStatus_PromotesProvenToContested(t *testing.T) {
	r := &Ruling{Contested: true}
	if got := r.Status(prove.StatusProven); got != prove.StatusContested {
		t.Errorf("status = %q, want %q", got, prove.StatusContested)
	}
	// An abstention must not be upgraded by the presence of a dispute.
	if got := r.Status(prove.StatusInsufficient); got != prove.StatusInsufficient {
		t.Errorf("status = %q, want the abstention preserved", got)
	}
	clean := &Ruling{}
	if got := clean.Status(prove.StatusProven); got != prove.StatusProven {
		t.Errorf("status = %q, want %q", got, prove.StatusProven)
	}
}

// The headline finding of Feature C: many sources, little independent support.
func TestCorroboration_DetectsSharedOrigin(t *testing.T) {
	f := newFake()
	// Six outlets assert the claim...
	var sources kg.Rows
	for i := 0; i < 6; i++ {
		sources = append(sources, kg.Row{
			"id": "s:outlet" + string(rune('a'+i)), "name": "Outlet",
			"kind": "news", "trust": 0.5,
		})
	}
	f.rows[kg.CourtDirectSupport.Name] = sources
	// ...but they all trace to one origin, so flow is one source's worth.
	f.rows[kg.CourtCorroboration.Name] = kg.Rows{{
		"flow": int64(50), "originCount": int64(1),
	}}
	e := NewEngine(f)

	c, err := e.corroboration(context.Background(), "c1")
	if err != nil {
		t.Fatalf("corroboration: %v", err)
	}
	if c.DirectSources != 6 {
		t.Errorf("DirectSources = %d, want 6", c.DirectSources)
	}
	if !c.Bottlenecked {
		t.Fatalf("six sources carrying one source's flow must be flagged as "+
			"bottlenecked (flow=%d)", c.IndependentFlow)
	}
	if !strings.Contains(c.Note, "share an upstream origin") {
		t.Errorf("note should name the finding: %q", c.Note)
	}
}

// The warning must not fire on ordinary evidence, or users learn to ignore it.
func TestCorroboration_DoesNotWarnOnGenuineIndependence(t *testing.T) {
	f := newFake()
	f.rows[kg.CourtDirectSupport.Name] = kg.Rows{
		{"id": "s:a", "name": "Filing Registry", "kind": "filing", "trust": 0.9},
		{"id": "s:b", "name": "Companies Registry", "kind": "filing", "trust": 0.9},
	}
	// Both contribute nearly their full capacity: genuinely independent.
	f.rows[kg.CourtCorroboration.Name] = kg.Rows{{
		"flow": int64(170), "originCount": int64(2),
	}}
	e := NewEngine(f)

	c, err := e.corroboration(context.Background(), "c1")
	if err != nil {
		t.Fatalf("corroboration: %v", err)
	}
	if c.Bottlenecked {
		t.Errorf("independent sources were flagged as bottlenecked (flow=%d): %q",
			c.IndependentFlow, c.Note)
	}
	if !strings.Contains(c.Note, "independent") {
		t.Errorf("note should confirm independence: %q", c.Note)
	}
}

// A single uncorroborated source is the poisoning case and must be called out.
func TestCorroboration_FlagsSingleSource(t *testing.T) {
	f := newFake()
	f.rows[kg.CourtDirectSupport.Name] = kg.Rows{
		{"id": "s:blog", "name": "Anonymous Finance Blog", "kind": "pressrelease", "trust": 0.1},
	}
	f.rows[kg.CourtCorroboration.Name] = kg.Rows{{"flow": int64(10), "originCount": int64(1)}}
	e := NewEngine(f)

	c, err := e.corroboration(context.Background(), "c1")
	if err != nil {
		t.Fatalf("corroboration: %v", err)
	}
	if !strings.Contains(c.Note, "Nothing corroborates") {
		t.Errorf("a single-source claim must be called out: %q", c.Note)
	}
}

// A corroboration failure must not sink the whole ruling: the disputes are
// still worth reporting.
func TestAdjudicate_SurvivesCorroborationFailure(t *testing.T) {
	f := newFake()
	f.rows[kg.CourtContradictions.Name] = kg.Rows{{
		"claimID": "c1", "counterText": "disputed", "sourceName": "Wire",
	}}
	f.errs[kg.CourtCorroboration.Name] = context.DeadlineExceeded
	e := NewEngine(f)

	r, err := e.Adjudicate(context.Background(), verdict())
	if err != nil {
		t.Fatalf("a corroboration failure must not fail the ruling: %v", err)
	}
	if !r.Contested {
		t.Error("disputes should still be reported")
	}
}

// A contradiction query failure is different: it would silently report "no
// dispute" on a contested claim, which is the dangerous direction.
func TestAdjudicate_PropagatesContradictionFailure(t *testing.T) {
	f := newFake()
	f.errs[kg.CourtContradictions.Name] = context.DeadlineExceeded
	e := NewEngine(f)

	if _, err := e.Adjudicate(context.Background(), verdict()); err == nil {
		t.Fatal("a failed contradiction query must surface, not report 'no dispute'")
	}
}

func TestAdjudicate_HandlesEmptyVerdict(t *testing.T) {
	e := NewEngine(newFake())
	r, err := e.Adjudicate(context.Background(), &prove.Verdict{Status: prove.StatusInsufficient})
	if err != nil {
		t.Fatalf("Adjudicate: %v", err)
	}
	if r.Contested || len(r.Disputes) != 0 {
		t.Error("an abstention has no chain to adjudicate")
	}
	if _, err := e.Adjudicate(context.Background(), nil); err != nil {
		t.Errorf("a nil verdict should be a no-op: %v", err)
	}
}

func TestChainClaims_DeduplicatesAndBounds(t *testing.T) {
	v := &prove.Verdict{Chains: []prove.Chain{
		{Steps: []prove.Step{{ClaimID: "a"}, {ClaimID: "b"}}},
		{Steps: []prove.Step{{ClaimID: "b"}, {ClaimID: "c"}}},
	}}
	got := chainClaims(v, 10)
	if len(got) != 3 {
		t.Fatalf("got %v, want 3 distinct claims", got)
	}
	if len(chainClaims(v, 2)) != 2 {
		t.Error("the bound was not applied")
	}
}
