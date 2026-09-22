package corpus

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Level classifies a question by what it demands of the retrieval system.
//
// The levels are ordered by how much of a graph you need. L1 is answerable by
// any keyword search. L2 needs multi-document composition. L4 needs time. L5
// needs to notice disagreement. L6 and L7 are the ones that separate the field:
// they reward not answering, and no top-k retriever can decline.
type Level string

const (
	L1SingleHop     Level = "L1_single_hop"
	L2MultiHop      Level = "L2_multi_hop"
	L3Relationship  Level = "L3_relationship"
	L4Temporal      Level = "L4_temporal"
	L5Contradiction Level = "L5_contradiction"
	L6Unanswerable  Level = "L6_unanswerable"
	L7Poisoned      Level = "L7_poisoned"
)

// AllLevels in benchmark-report order.
var AllLevels = []Level{
	L1SingleHop, L2MultiHop, L3Relationship, L4Temporal,
	L5Contradiction, L6Unanswerable, L7Poisoned,
}

// Question is one benchmark item with its ground truth.
type Question struct {
	ID    string `json:"id"`
	Level Level  `json:"level"`
	Text  string `json:"text"`

	// Answer is the expected answer in natural language. Empty when the correct
	// behaviour is to abstain.
	Answer string `json:"answer,omitempty"`

	// MustAbstain marks a question where any confident answer is wrong. This is
	// the metric no baseline can score on, because a top-k retriever always
	// returns something.
	MustAbstain bool `json:"mustAbstain"`

	// MustContain are strings the answer is required to include, used for
	// automatic grading without an LLM judge on the objective levels.
	MustContain []string `json:"mustContain,omitempty"`

	// MustNotContain are strings whose presence marks a failure - a poisoned
	// assertion that reached the answer, most importantly.
	MustNotContain []string `json:"mustNotContain,omitempty"`

	// AsOf scopes a temporal question.
	AsOf time.Time `json:"asOf,omitempty"`

	// Hops is the minimum number of ownership steps the answer requires, which
	// is what makes the multi-hop degradation curve plottable.
	Hops int `json:"hops,omitempty"`

	// Why explains, for the benchmark report, what this item is testing.
	Why string `json:"why"`
}

// QuestionSet is the generated benchmark.
type QuestionSet struct {
	Seed      int64      `json:"seed"`
	Questions []Question `json:"questions"`
}

// ByLevel groups questions for per-level reporting.
func (qs QuestionSet) ByLevel() map[Level][]Question {
	out := map[Level][]Question{}
	for _, q := range qs.Questions {
		out[q.Level] = append(out[q.Level], q)
	}
	return out
}

// Counts returns per-level question counts in report order.
func (qs QuestionSet) Counts() []string {
	by := qs.ByLevel()
	var out []string
	for _, l := range AllLevels {
		out = append(out, fmt.Sprintf("%s=%d", l, len(by[l])))
	}
	return out
}

// Questions generates the benchmark from the corpus.
//
// Every expected answer is read off the ground-truth world rather than written
// by hand. That is what makes the set regenerable at a different seed or scale
// without re-annotating, and what stops a question from silently disagreeing
// with the corpus after a generator change.
func (c *Corpus) Questions() QuestionSet {
	qs := QuestionSet{Seed: c.World.Seed}
	w := c.World
	n := 0
	id := func(l Level) string { n++; return fmt.Sprintf("%s-%03d", shortLevel(l), n) }

	withheld := map[string]bool{}
	for _, h := range c.Withheld {
		withheld[h.Holder+"|"+h.Issuer] = true
	}
	restated := map[string]Restatement{}
	for _, r := range c.Restated {
		restated[r.Issuer] = r
	}
	contradicted := map[string]Contradiction{}
	for _, x := range c.Contradicted {
		contradicted[x.Issuer] = x
	}

	// ── L1: single hop ───────────────────────────────────────────────────────
	// Answerable from one document. The floor: a system that fails here is
	// broken, and the level exists to prove the corpus is ingestible at all.
	for _, o := range w.Officers {
		if len(qs.Questions) > 400 {
			break
		}
		qs.Questions = append(qs.Questions, Question{
			ID:          id(L1SingleHop),
			Level:       L1SingleHop,
			Text:        fmt.Sprintf("Who serves as %s of %s?", o.Role, o.Company),
			Answer:      o.Person,
			MustContain: []string{o.Person},
			Why:         "single document, direct lookup",
		})
	}

	// ── L2: multi-hop ────────────────────────────────────────────────────────
	// The answer requires composing every majority holding along a chain with
	// the definitional rule from a separate reference document. No single
	// passage contains it, so passage retrieval cannot succeed by luck.
	for _, ch := range w.Chains {
		if !chainIntact(ch, withheld) {
			continue // its evidence was withheld; it belongs to L6
		}
		qs.Questions = append(qs.Questions, Question{
			ID:          id(L2MultiHop),
			Level:       L2MultiHop,
			Text:        fmt.Sprintf("Which entity ultimately controls %s?", ch.Target()),
			Answer:      ch.Ultimate(),
			MustContain: []string{ch.Ultimate()},
			Hops:        ch.Hops(),
			Why: fmt.Sprintf("%d-hop ownership chain composed with the >50%% control rule "+
				"from a separate reference document", ch.Hops()),
		})

		// The person at the top: one further hop through an officer filing.
		if o, ok := w.OfficerOf(ch.Ultimate()); ok {
			qs.Questions = append(qs.Questions, Question{
				ID:    id(L2MultiHop),
				Level: L2MultiHop,
				Text: fmt.Sprintf("Which individual is an officer of the entity that ultimately controls %s?",
					ch.Target()),
				Answer:      o.Person,
				MustContain: []string{o.Person},
				Hops:        ch.Hops() + 1,
				Why:         "ownership chain plus an officer filing at the top",
			})
		}
	}

	// ── L3: relationship ─────────────────────────────────────────────────────
	// Tests whether the system reports the *shape* of a link, not just its
	// endpoints. Direct-versus-intermediated is the distinction that matters in
	// an investigation and the one a bag of passages loses.
	for _, ch := range w.Chains {
		if ch.Hops() < 2 || !chainIntact(ch, withheld) {
			continue
		}
		qs.Questions = append(qs.Questions, Question{
			ID:    id(L3Relationship),
			Level: L3Relationship,
			Text: fmt.Sprintf("Is the control of %s by %s direct, or held through an intermediate entity?",
				ch.Target(), ch.Ultimate()),
			Answer:      fmt.Sprintf("Through %d intermediate entities", ch.Hops()-1),
			MustContain: []string{ch.Links[1]},
			Hops:        ch.Hops(),
			Why:         "requires naming the intermediary, not just the endpoints",
		})
	}

	// ── L4: temporal ─────────────────────────────────────────────────────────
	// A later amendment supersedes the original figure. A system with no notion
	// of validity time returns the current value and is wrong; the correct
	// answer is the value that held on the as-of date.
	for issuer, r := range restated {
		h, ok := w.HoldingOf(issuer)
		if !ok {
			continue
		}
		qs.Questions = append(qs.Questions, Question{
			ID:    id(L4Temporal),
			Level: L4Temporal,
			Text: fmt.Sprintf("What percentage of %s did %s hold as of %s?",
				issuer, h.Holder, r.AsOf.Format("2 January 2006")),
			Answer:         fmt.Sprintf("%.1f percent", r.OldPercent),
			MustContain:    []string{fmt.Sprintf("%.1f", r.OldPercent)},
			MustNotContain: []string{fmt.Sprintf("%.1f", r.NewPercent)},
			AsOf:           r.AsOf,
			Why: fmt.Sprintf("a later amendment restates this to %.1f%%; the as-of date "+
				"precedes the amendment, so the original figure is correct", r.NewPercent),
		})
	}

	// ── L5: contradiction ────────────────────────────────────────────────────
	// Two sources disagree. Scoring rewards surfacing the dispute and siding
	// with the higher-trust filing over the unverified news report.
	for issuer, x := range contradicted {
		h, ok := w.HoldingOf(issuer)
		if !ok {
			continue
		}
		qs.Questions = append(qs.Questions, Question{
			ID:    id(L5Contradiction),
			Level: L5Contradiction,
			Text: fmt.Sprintf("What percentage of %s does %s hold, and do sources agree?",
				issuer, h.Holder),
			Answer:      fmt.Sprintf("%.1f percent per the regulatory filing; disputed", x.TruePercent),
			MustContain: []string{fmt.Sprintf("%.1f", x.TruePercent)},
			Why: fmt.Sprintf("%s reports %.1f%%, contradicting the filing's %.1f%%; "+
				"the dispute should be surfaced, not silently resolved",
				x.FalseSource, x.FalsePercent, x.TruePercent),
		})
	}

	// ── L6: unanswerable ─────────────────────────────────────────────────────
	// The evidence for a link in the chain was never written. The correct
	// behaviour is to abstain and say where the gap is.
	//
	// This is the level that separates ARGUS from every baseline, because a
	// top-k retriever structurally cannot decline: it always has a best match,
	// and it will present it.
	for _, h := range c.Withheld {
		qs.Questions = append(qs.Questions, Question{
			ID:    id(L6Unanswerable),
			Level: L6Unanswerable,
			Text: fmt.Sprintf("What percentage of %s does %s hold?",
				h.Issuer, h.Holder),
			MustAbstain: true,
			Why:         "the filing stating this holding was deliberately withheld from the corpus",
		})
	}
	// And the chains those gaps break.
	for _, ch := range w.Chains {
		if chainIntact(ch, withheld) {
			continue
		}
		qs.Questions = append(qs.Questions, Question{
			ID:          id(L6Unanswerable),
			Level:       L6Unanswerable,
			Text:        fmt.Sprintf("Which entity ultimately controls %s?", ch.Target()),
			MustAbstain: true,
			Hops:        ch.Hops(),
			Why:         "a link in this ownership chain has no supporting document",
		})
	}

	// ── L7: poisoned ─────────────────────────────────────────────────────────
	// A false claim from an uncorroborated, low-trust source. Scoring is
	// asymmetric and deliberately harsh: repeating the false assertion is a
	// failure even if the rest of the answer is correct.
	for _, p := range c.Poisoned {
		qs.Questions = append(qs.Questions, Question{
			ID:             id(L7Poisoned),
			Level:          L7Poisoned,
			Text:           fmt.Sprintf("Is %s under criminal investigation?", p.Company),
			MustAbstain:    true,
			MustNotContain: []string{"under active criminal investigation", "securities fraud"},
			Why: fmt.Sprintf("only %s asserts this, with no corroboration; "+
				"repeating it is a failure", p.SourceName),
		})
	}

	sort.SliceStable(qs.Questions, func(i, j int) bool {
		return qs.Questions[i].ID < qs.Questions[j].ID
	})
	return qs
}

// chainIntact reports whether every link in a chain has surviving evidence.
func chainIntact(ch Chain, withheld map[string]bool) bool {
	for i := 0; i < len(ch.Links)-1; i++ {
		if withheld[ch.Links[i]+"|"+ch.Links[i+1]] {
			return false
		}
	}
	return true
}

func shortLevel(l Level) string {
	if i := strings.IndexByte(string(l), '_'); i > 0 {
		return string(l)[:i]
	}
	return string(l)
}
