package kg

import (
	"math"
	"strings"
	"testing"
)

func TestValidate_RejectsWriteClauseInReadTemplate(t *testing.T) {
	cases := []struct {
		name   string
		cypher string
	}{
		{"detach delete", "MATCH (c:Claim {id:$id}) DETACH DELETE c"},
		{"set", "MATCH (c:Claim {id:$id}) SET c.conf = 1.0 RETURN c"},
		{"create", "CREATE (c:Claim {id:$id}) RETURN c"},
		{"merge", "MERGE (c:Claim {id:$id}) RETURN c"},
		{"remove", "MATCH (c:Claim {id:$id}) REMOVE c.conf RETURN c"},
		{"lowercase", "match (c:Claim {id:$id}) detach delete c"},
		{"load csv", "LOAD CSV FROM $id AS row RETURN row"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tpl := Template{Name: "t", Cypher: tc.cypher, Mode: ModeRead, Params: []string{"id"}}
			err := tpl.Validate()
			if err == nil {
				t.Fatalf("expected a read template containing %q to be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), "write clause") {
				t.Fatalf("expected a write-clause error, got: %v", err)
			}
		})
	}
}

func TestValidate_AllowsWriteClauseInWriteTemplate(t *testing.T) {
	tpl := Template{
		Name:   "claim.upsert",
		Cypher: "MERGE (c:Claim {id:$id}) SET c.conf = $conf RETURN c",
		Mode:   ModeWrite,
		Params: []string{"id", "conf"},
	}
	if err := tpl.Validate(); err != nil {
		t.Fatalf("write template rejected: %v", err)
	}
}

func TestValidate_WriteKeywordInCommentIsNotAWrite(t *testing.T) {
	tpl := Template{
		Name: "safe.comment",
		Cypher: `// we deliberately do not DELETE here
/* nor SET anything */
MATCH (c:Claim {id:$id}) RETURN c.text`,
		Mode:   ModeRead,
		Params: []string{"id"},
	}
	if err := tpl.Validate(); err != nil {
		t.Fatalf("comment-only write keyword should not trip the validator: %v", err)
	}
}

func TestValidate_UndeclaredParameter(t *testing.T) {
	tpl := Template{
		Name:   "t",
		Cypher: "MATCH (c:Claim {id:$id}) WHERE c.conf > $floor RETURN c",
		Mode:   ModeRead,
		Params: []string{"id"}, // $floor missing
	}
	err := tpl.Validate()
	if err == nil || !strings.Contains(err.Error(), "floor") {
		t.Fatalf("expected an undeclared-parameter error for $floor, got: %v", err)
	}
}

func TestValidate_UnusedDeclaredParameter(t *testing.T) {
	tpl := Template{
		Name:   "t",
		Cypher: "MATCH (c:Claim {id:$id}) RETURN c",
		Mode:   ModeRead,
		Params: []string{"id", "unused"},
	}
	err := tpl.Validate()
	if err == nil || !strings.Contains(err.Error(), "unused") {
		t.Fatalf("expected an unused-parameter error, got: %v", err)
	}
}

func TestRender_MissingParameterIsAnError(t *testing.T) {
	tpl := Template{
		Name:   "t",
		Cypher: "MATCH (c:Claim {id:$id}) WHERE c.conf > $floor RETURN c",
		Mode:   ModeRead,
		Params: []string{"id", "floor"},
	}
	if _, err := tpl.render(Params{"id": "x"}); err == nil {
		t.Fatal("expected render to fail when a declared parameter is absent")
	}
}

// A classic Cypher injection payload must survive as an inert string. render
// returns the template unchanged, so the payload can only ever arrive at the
// server as a bound parameter value, never as query text.
func TestRender_InjectionPayloadNeverEntersQueryText(t *testing.T) {
	tpl := Template{
		Name:   "claim.byID",
		Cypher: "MATCH (c:Claim {id:$id}) RETURN c.text",
		Mode:   ModeRead,
		Params: []string{"id"},
	}
	payload := `x'}) DETACH DELETE c //`
	out, err := tpl.render(Params{"id": payload})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out != tpl.Cypher {
		t.Fatalf("render must not interpolate; got %q", out)
	}
	if strings.Contains(out, "DELETE") {
		t.Fatal("injection payload leaked into query text")
	}
}

func TestWeightRoundTrip(t *testing.T) {
	for _, conf := range []float64{1.0, 0.95, 0.8, 0.5, 0.2, 0.05} {
		w := WeightFromConfidence(conf)
		got := ConfidenceFromWeight(w)
		if math.Abs(got-conf) > 1e-3 {
			t.Errorf("conf %.4f -> w %d -> %.4f (drift %.5f exceeds fixed-point tolerance)",
				conf, w, got, math.Abs(got-conf))
		}
	}
}

// A certain step must cost nothing, so that adding well-evidenced hops to a
// derivation does not make it look less likely than a shorter speculative one.
func TestWeightOfCertaintyIsZero(t *testing.T) {
	if w := WeightFromConfidence(1.0); w != 0 {
		t.Fatalf("confidence 1.0 should weigh 0, got %d", w)
	}
}

func TestWeightIsMonotonic(t *testing.T) {
	prev := int64(-1)
	for _, conf := range []float64{1.0, 0.9, 0.7, 0.5, 0.3, 0.1, 0.01} {
		w := WeightFromConfidence(conf)
		if w <= prev && prev != -1 {
			t.Fatalf("weight must increase as confidence falls: conf %.2f gave %d after %d", conf, w, prev)
		}
		prev = w
	}
}

// Zero and negative confidences must not produce +Inf, which would serialise as
// a non-numeric weight and break algo.SPpaths.
func TestWeightHandlesDegenerateConfidence(t *testing.T) {
	for _, conf := range []float64{0, -1, math.SmallestNonzeroFloat64} {
		w := WeightFromConfidence(conf)
		if w <= 0 || w > 1<<40 {
			t.Fatalf("confidence %v produced an unusable weight %d", conf, w)
		}
	}
}

func TestForkNameGuard(t *testing.T) {
	if IsForkName("argus") {
		t.Fatal("the primary graph must never look like a fork")
	}
	if !IsForkName(ForkPrefix + "01") {
		t.Fatal("fork names must be recognised so the reaper can clean them up")
	}
}
