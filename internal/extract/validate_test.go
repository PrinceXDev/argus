package extract

import (
	"strings"
	"testing"
)

const sample = `Zeta Holdings LLC reported a 60% beneficial ownership stake in
Acme Corporation as of March 14, 2019. The filing was signed by
Dana Reyes, Chief Financial Officer of Zeta Holdings LLC.`

func TestFindSpan_Exact(t *testing.T) {
	q := "60% beneficial ownership stake"
	s, ok := FindSpan(sample, q)
	if !ok {
		t.Fatal("exact quote should be found")
	}
	if got := sample[s.Start:s.End]; got != q {
		t.Fatalf("span %v yields %q, want %q", s, got, q)
	}
}

// Models routinely collapse newlines when quoting from a filing. That is a
// formatting artefact, not a fabrication, so it must still resolve - and must
// resolve to offsets into the *original* text.
func TestFindSpan_WhitespaceNormalised(t *testing.T) {
	q := "60% beneficial ownership stake in Acme Corporation"
	s, ok := FindSpan(sample, q)
	if !ok {
		t.Fatal("quote spanning a newline should be found after normalisation")
	}
	got := normaliseSpace(sample[s.Start:s.End])
	if got != q {
		t.Fatalf("span %v yields %q, want %q", s, got, q)
	}
}

// The guard's whole purpose: an invented quote must not resolve.
func TestFindSpan_RejectsFabricatedQuote(t *testing.T) {
	for _, q := range []string{
		"Zeta Holdings LLC reported a 90% beneficial ownership stake", // number altered
		"Zeta Holdings LLC did not report any ownership stake",        // negated
		"Acme Corporation was acquired by Omega Partners",             // wholly invented
	} {
		if _, ok := FindSpan(sample, q); ok {
			t.Errorf("fabricated quote passed the span check: %q", q)
		}
	}
}

// Fuzzy matching is deliberately absent. A near-miss must fail, because an
// edit-distance match would let a model change a number and still pass.
func TestFindSpan_DoesNotFuzzyMatch(t *testing.T) {
	if _, ok := FindSpan(sample, "60% beneficial ownership stak"); !ok {
		// A strict prefix IS a substring, so this one legitimately matches.
		t.Log("prefix matched, as expected for a true substring")
	}
	if _, ok := FindSpan(sample, "61% beneficial ownership stake"); ok {
		t.Error("a single altered digit must not match")
	}
}

func TestFindSpan_EmptyInputs(t *testing.T) {
	if _, ok := FindSpan(sample, ""); ok {
		t.Error("empty quote must not match")
	}
	if _, ok := FindSpan("", "anything"); ok {
		t.Error("empty text must not match")
	}
	if _, ok := FindSpan(sample, "   \n  "); ok {
		t.Error("whitespace-only quote must not match")
	}
}

func TestSpanValid(t *testing.T) {
	n := len(sample)
	cases := []struct {
		s    Span
		want bool
	}{
		{Span{0, 5}, true},
		{Span{0, 0}, false},     // empty
		{Span{5, 3}, false},     // inverted
		{Span{-1, 5}, false},    // negative
		{Span{0, n + 1}, false}, // past the end
	}
	for _, c := range cases {
		if got := c.s.Valid(n); got != c.want {
			t.Errorf("Span%v.Valid(%d) = %v, want %v", c.s, n, got, c.want)
		}
	}
}

// ─── calibration ─────────────────────────────────────────────────────────────

// A model's self-reported confidence is not a probability. The kind's ceiling
// must dominate it, always downward.
func TestCalibrate_CeilingDominatesModelClaim(t *testing.T) {
	cases := []struct {
		kind     EdgeKind
		reported float64
		want     float64
	}{
		{KindStated, 0.99, 0.98},
		{KindDefinitional, 1.0, 0.95},
		{KindInference, 0.95, 0.80},
		{KindAssumption, 0.99, 0.55}, // the important one
		{KindInference, 0.60, 0.60},  // below the ceiling, left alone
	}
	for _, c := range cases {
		if got := c.kind.Calibrate(c.reported); got != c.want {
			t.Errorf("%s.Calibrate(%.2f) = %.2f, want %.2f", c.kind, c.reported, got, c.want)
		}
	}
}

func TestCalibrate_NeverRaisesConfidence(t *testing.T) {
	for kind := range kindProfiles {
		for _, reported := range []float64{0.1, 0.3, 0.5, 0.7, 0.9} {
			if got := kind.Calibrate(reported); got > reported {
				t.Errorf("%s.Calibrate(%.2f) raised confidence to %.2f", kind, reported, got)
			}
		}
	}
}

func TestCalibrate_FloorKeepsWeightFinite(t *testing.T) {
	if got := KindAssumption.Calibrate(0); got < 0.05 {
		t.Errorf("calibrate(0) = %v; a zero confidence would make -ln(conf) unbounded", got)
	}
}

// An unrecognised kind must degrade to the weakest band rather than being
// treated as strong or silently dropped.
func TestCalibrate_UnknownKindDegradesToAssumption(t *testing.T) {
	unknown := EdgeKind("wishful_thinking")
	if unknown.Valid() {
		t.Fatal("test kind should not be recognised")
	}
	if got, want := unknown.Calibrate(0.99), kindProfiles[KindAssumption].MaxConf; got != want {
		t.Errorf("unknown kind calibrated to %.2f, want the assumption ceiling %.2f", got, want)
	}
	if got, want := unknown.LeapCost(), kindProfiles[KindAssumption].Leap; got != want {
		t.Errorf("unknown kind leap = %d, want %d", got, want)
	}
}

// Speculative steps must cost more leap budget than direct ones, or the
// speculation slider does nothing.
func TestLeapCostIncreasesWithSpeculation(t *testing.T) {
	if KindStated.LeapCost() > KindInference.LeapCost() {
		t.Error("a stated step must not cost more than an inference")
	}
	if KindInference.LeapCost() >= KindAssumption.LeapCost() {
		t.Error("an assumption must cost more leap budget than an inference")
	}
}

// ─── validation ──────────────────────────────────────────────────────────────

func goodExtraction() Extraction {
	return Extraction{
		Entities: []Entity{
			{Name: "Zeta Holdings LLC", Type: EntityCompany, Quote: "Zeta Holdings LLC"},
			{Name: "Acme Corporation", Type: EntityCompany, Quote: "Acme Corporation"},
			{Name: "Dana Reyes", Type: EntityPerson, Quote: "Dana Reyes"},
		},
		Claims: []Claim{
			{
				Ref: "c1", Text: "Zeta Holdings LLC held 60% of Acme Corporation on 2019-03-14.",
				Quote:      "60% beneficial ownership stake",
				Entities:   []string{"Zeta Holdings LLC", "Acme Corporation"},
				Confidence: 0.95, ValidFrom: "2019-03-14",
			},
			{
				Ref: "c2", Text: "Dana Reyes was CFO of Zeta Holdings LLC.",
				Quote:      "Dana Reyes, Chief Financial Officer",
				Entities:   []string{"Dana Reyes", "Zeta Holdings LLC"},
				Confidence: 0.9,
			},
		},
		Entails: []Entailment{
			{From: "c1", To: "c2", Kind: KindInference, Confidence: 0.9, Rationale: "officer of the holder"},
		},
		Relations: []Relation{
			{From: "Zeta Holdings LLC", To: "Acme Corporation", Type: RelOwns,
				Amount: 60, Quote: "60% beneficial ownership stake"},
		},
	}
}

func TestValidate_HappyPath(t *testing.T) {
	v, rep := Validate(sample, goodExtraction())
	if len(rep.Rejections) != 0 {
		t.Fatalf("unexpected rejections: %v", rep.Rejections)
	}
	if len(v.Entities) != 3 || len(v.Claims) != 2 || len(v.Entails) != 1 || len(v.Relations) != 1 {
		t.Fatalf("kept %d entities, %d claims, %d entails, %d relations",
			len(v.Entities), len(v.Claims), len(v.Entails), len(v.Relations))
	}
	// The entailment was declared at 0.9 but is an inference, ceiling 0.80.
	if got := v.Entails[0].Confidence; got != 0.80 {
		t.Errorf("entailment confidence = %.2f, want the inference ceiling 0.80", got)
	}
	if got := v.Entails[0].Leap; got != 2 {
		t.Errorf("entailment leap = %d, want 2", got)
	}
	// Every span must resolve to real text.
	for _, c := range v.Claims {
		if !c.Span.Valid(len(sample)) {
			t.Errorf("claim %s has an invalid span %v", c.Ref, c.Span)
		}
	}
}

func TestValidate_RejectsClaimWithFabricatedQuote(t *testing.T) {
	x := goodExtraction()
	x.Claims[0].Quote = "Zeta Holdings LLC held a 95% stake"
	v, rep := Validate(sample, x)

	for _, c := range v.Claims {
		if c.Ref == "c1" {
			t.Fatal("a claim with an unfindable quote must not survive")
		}
	}
	var found bool
	for _, r := range rep.Rejections {
		if r.Kind == "span" {
			found = true
		}
	}
	if !found {
		t.Error("expected a span rejection to be recorded")
	}
}

// An entailment pointing at a rejected claim must be dropped, or the graph
// would contain an inference edge to nothing.
func TestValidate_DropsEntailmentToRejectedClaim(t *testing.T) {
	x := goodExtraction()
	x.Claims[1].Quote = "invented text that is not in the source"
	v, _ := Validate(sample, x)

	if len(v.Entails) != 0 {
		t.Fatalf("entailment survived despite its target being rejected: %+v", v.Entails)
	}
}

func TestValidate_RejectsClaimAboutNoKnownEntity(t *testing.T) {
	x := goodExtraction()
	x.Claims[0].Entities = []string{"Ghost Industries"}
	v, rep := Validate(sample, x)

	for _, c := range v.Claims {
		if c.Ref == "c1" {
			t.Fatal("a claim naming no surviving entity must be rejected as unanchorable")
		}
	}
	var found bool
	for _, r := range rep.Rejections {
		if r.Kind == "entity" {
			found = true
		}
	}
	if !found {
		t.Error("expected an entity rejection")
	}
}

func TestValidate_RejectsUnknownEntityType(t *testing.T) {
	x := goodExtraction()
	x.Entities[0].Type = EntityType("Spaceship")
	_, rep := Validate(sample, x)

	var found bool
	for _, r := range rep.Rejections {
		if r.Kind == "type" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a type rejection, got %v", rep.Rejections)
	}
}

func TestValidate_RejectsSelfEntailment(t *testing.T) {
	x := goodExtraction()
	x.Entails = []Entailment{{From: "c1", To: "c1", Kind: KindStated, Confidence: 1}}
	v, _ := Validate(sample, x)
	if len(v.Entails) != 0 {
		t.Error("a self-entailment would be a zero-length cycle in the derivation graph")
	}
}

func TestValidate_DeduplicatesClaimRefs(t *testing.T) {
	x := goodExtraction()
	dup := x.Claims[0]
	x.Claims = append(x.Claims, dup)
	v, rep := Validate(sample, x)

	n := 0
	for _, c := range v.Claims {
		if c.Ref == "c1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("kept %d claims with ref c1, want 1", n)
	}
	var found bool
	for _, r := range rep.Rejections {
		if r.Kind == "ref" {
			found = true
		}
	}
	if !found {
		t.Error("expected a duplicate-ref rejection")
	}
}

func TestReport_RejectionRate(t *testing.T) {
	r := Report{ClaimsKept: 3, Rejections: make([]Rejection, 1)}
	if got := r.RejectionRate(); got < 0.24 || got > 0.26 {
		t.Errorf("RejectionRate = %.3f, want 0.25", got)
	}
	if got := (Report{}).RejectionRate(); got != 0 {
		t.Errorf("empty report rate = %v, want 0", got)
	}
}

func TestReport_MergeAndString(t *testing.T) {
	a := Report{ChunksProcessed: 1, ClaimsKept: 2, EntitiesKept: 3}
	a.Merge(Report{ChunksProcessed: 1, ClaimsKept: 1, Rejections: []Rejection{{Kind: "span"}}})
	if a.ChunksProcessed != 2 || a.ClaimsKept != 3 {
		t.Fatalf("merge produced %+v", a)
	}
	if !strings.Contains(a.String(), "rejected") {
		t.Errorf("report string should surface rejections: %s", a.String())
	}
}
