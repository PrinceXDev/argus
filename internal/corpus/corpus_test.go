package corpus

import (
	"strings"
	"testing"
)

// A benchmark you cannot regenerate byte-for-byte is not a benchmark.
func TestGenerate_IsDeterministic(t *testing.T) {
	a := Generate(DefaultOptions())
	b := Generate(DefaultOptions())

	if len(a.Companies) != len(b.Companies) || len(a.Holdings) != len(b.Holdings) {
		t.Fatalf("world sizes differ between runs: %d/%d vs %d/%d",
			len(a.Companies), len(a.Holdings), len(b.Companies), len(b.Holdings))
	}
	for i := range a.Companies {
		if a.Companies[i] != b.Companies[i] {
			t.Fatalf("company %d differs: %+v vs %+v", i, a.Companies[i], b.Companies[i])
		}
	}
	for i := range a.Holdings {
		if a.Holdings[i] != b.Holdings[i] {
			t.Fatalf("holding %d differs: %+v vs %+v", i, a.Holdings[i], b.Holdings[i])
		}
	}
}

func TestGenerate_DifferentSeedsDiffer(t *testing.T) {
	o := DefaultOptions()
	a := Generate(o)
	o.Seed = 99
	b := Generate(o)
	if a.Companies[0] == b.Companies[0] {
		t.Error("different seeds produced identical worlds")
	}
}

func TestBuild_IsDeterministic(t *testing.T) {
	a := Build(Generate(DefaultOptions()), DefaultPerturbations())
	b := Build(Generate(DefaultOptions()), DefaultPerturbations())
	if len(a.Docs) != len(b.Docs) {
		t.Fatalf("document counts differ: %d vs %d", len(a.Docs), len(b.Docs))
	}
	for i := range a.Docs {
		if a.Docs[i].Text != b.Docs[i].Text {
			t.Fatalf("document %d text differs between runs", i)
		}
	}
}

// Every link must be a majority stake, so the definitional step is always
// required and the control conclusion is always derivable.
func TestGenerate_EveryHoldingIsControlling(t *testing.T) {
	w := Generate(DefaultOptions())
	if len(w.Holdings) == 0 {
		t.Fatal("no holdings generated")
	}
	for _, h := range w.Holdings {
		if h.Percent <= 50 {
			t.Errorf("%s holds %.1f%% of %s; a non-majority link breaks the control chain",
				h.Holder, h.Percent, h.Issuer)
		}
		if h.Percent > 100 {
			t.Errorf("%s holds %.1f%% of %s, which is impossible", h.Holder, h.Percent, h.Issuer)
		}
	}
}

func TestGenerate_ChainsAreDeepEnoughToBeMultiHop(t *testing.T) {
	w := Generate(DefaultOptions())
	if len(w.Chains) == 0 {
		t.Fatal("no chains generated")
	}
	for _, ch := range w.Chains {
		if ch.Hops() < 2 {
			t.Errorf("chain %v has %d hops; single-hop chains do not test composition",
				ch.Links, ch.Hops())
		}
		if ch.Ultimate() == ch.Target() {
			t.Error("chain starts and ends at the same entity")
		}
	}
}

// The ring exists only as graph structure: a closed transfer loop between
// companies sharing an address, described by no single document.
func TestGenerate_RingIsClosedAndSharesAnAddress(t *testing.T) {
	w := Generate(DefaultOptions())
	if len(w.Ring) < 3 {
		t.Skip("ring disabled")
	}
	addr := ""
	for _, name := range w.Ring {
		co, ok := w.CompanyByName(name)
		if !ok {
			t.Fatalf("ring member %q is not in the company list", name)
		}
		if addr == "" {
			addr = co.Address
		} else if co.Address != addr {
			t.Errorf("ring member %s has address %q, want the shared %q", name, co.Address, addr)
		}
	}

	// Every ring member must both send and receive, or the loop is not closed
	// and community detection has nothing to find.
	sends, receives := map[string]bool{}, map[string]bool{}
	inRing := map[string]bool{}
	for _, n := range w.Ring {
		inRing[n] = true
	}
	for _, tr := range w.Transfers {
		if inRing[tr.From] && inRing[tr.To] {
			sends[tr.From] = true
			receives[tr.To] = true
		}
	}
	for _, n := range w.Ring {
		if !sends[n] || !receives[n] {
			t.Errorf("ring member %s does not both send and receive; the loop is open", n)
		}
	}
}

func TestGenerate_CompanyNamesAreUnique(t *testing.T) {
	w := Generate(DefaultOptions())
	seen := map[string]bool{}
	for _, c := range w.Companies {
		if seen[c.Name] {
			t.Errorf("duplicate company name %q would merge two entities in the graph", c.Name)
		}
		seen[c.Name] = true
	}
}

// ─── perturbations ───────────────────────────────────────────────────────────

// The withheld filings must genuinely not exist in the corpus, or the
// "unanswerable" questions are answerable and the abstention metric is a lie.
func TestBuild_WithheldEvidenceIsGenuinelyAbsent(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	if len(c.Withheld) == 0 {
		t.Fatal("no evidence was withheld")
	}
	all := allText(c)
	for _, h := range c.Withheld {
		// The filing template always renders "<holder> reports beneficial
		// ownership of <pct> percent ... of <issuer>". If that sentence is
		// absent, the fact is genuinely unstated.
		needle := strings.ToLower(h.Holder) + " reports beneficial ownership"
		if strings.Contains(strings.ToLower(all), needle) {
			// It may legitimately appear for a different issuer, so check the
			// pair rather than the holder alone.
			for _, d := range c.Docs {
				lt := strings.ToLower(d.Text)
				if strings.Contains(lt, needle) && strings.Contains(lt, strings.ToLower(h.Issuer)) {
					t.Errorf("withheld holding %s->%s still appears in document %q",
						h.Holder, h.Issuer, d.Title)
				}
			}
		}
	}
}

func TestBuild_PoisonIsPresentAndLowTrust(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	if len(c.Poisoned) == 0 {
		t.Fatal("no poison injected")
	}
	for _, p := range c.Poisoned {
		var found *Doc
		for i := range c.Docs {
			if strings.Contains(c.Docs[i].Text, p.FalseText) {
				found = &c.Docs[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("poisoned claim %q is not in any document", p.FalseText)
		}
		// The whole point of the L7 class is that source trust, not span
		// integrity, is what must contain this.
		if found.SourceTrust > 0.3 {
			t.Errorf("poison carried by a source with trust %.2f; it must be low-trust "+
				"for trust-weighting to be the thing under test", found.SourceTrust)
		}
	}
}

// The amendment must be published after the as-of date, or the temporal
// question has no correct earlier answer to find.
func TestBuild_RestatementsPostdateTheAsOfDate(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	if len(c.Restated) == 0 {
		t.Fatal("no restatements generated")
	}
	for _, r := range c.Restated {
		if !r.RestatedAt.After(r.AsOf) {
			t.Errorf("%s: amendment at %s does not postdate the as-of date %s",
				r.Issuer, r.RestatedAt.Format("2006-01-02"), r.AsOf.Format("2006-01-02"))
		}
		if r.OldPercent == r.NewPercent {
			t.Errorf("%s: restatement did not change the figure", r.Issuer)
		}
	}
}

func TestBuild_ContradictionsDisagreeButBothImplyControl(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	for _, x := range c.Contradicted {
		if x.TruePercent == x.FalsePercent {
			t.Errorf("%s: contradiction does not contradict", x.Issuer)
		}
		// Both above 50: the dispute is about the figure, not the conclusion.
		// That is the realistic and harder case.
		if x.FalsePercent <= 50 {
			t.Errorf("%s: the false figure %.1f%% flips the control conclusion, "+
				"making the dispute trivially detectable", x.Issuer, x.FalsePercent)
		}
	}
}

// The control rule lives in its own document on purpose: it forces genuine
// multi-document composition instead of letting one passage answer the question.
func TestBuild_ControlRuleIsInASeparateDocument(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	var ruleDocs int
	for _, d := range c.Docs {
		if strings.Contains(d.Text, "more than 50 percent of the voting shares") {
			ruleDocs++
			if d.SourceKind != "reference" {
				t.Errorf("the control rule appears in a %q document", d.SourceKind)
			}
		}
	}
	if ruleDocs != 1 {
		t.Errorf("the control rule appears in %d documents, want exactly 1", ruleDocs)
	}
}

// ─── questions ───────────────────────────────────────────────────────────────

func TestQuestions_CoverEveryLevel(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	by := c.Questions().ByLevel()
	for _, l := range AllLevels {
		if len(by[l]) == 0 {
			t.Errorf("no questions generated for %s", l)
		}
	}
}

// The two levels that separate ARGUS from the baselines must be well populated,
// or the headline result rests on a handful of items.
func TestQuestions_HardLevelsAreWellPopulated(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	by := c.Questions().ByLevel()
	if n := len(by[L6Unanswerable]); n < 4 {
		t.Errorf("only %d unanswerable questions; the abstention metric needs more", n)
	}
	if n := len(by[L7Poisoned]); n < 3 {
		t.Errorf("only %d poisoned questions; the robustness metric needs more", n)
	}
}

// An unanswerable question that is accidentally answerable would silently
// invert the abstention metric.
func TestQuestions_UnanswerableHaveNoExpectedAnswer(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	for _, q := range c.Questions().Questions {
		if q.Level != L6Unanswerable && q.Level != L7Poisoned {
			continue
		}
		if !q.MustAbstain {
			t.Errorf("%s: %s must be marked MustAbstain", q.ID, q.Level)
		}
		if q.Answer != "" {
			t.Errorf("%s: an abstention question must not carry an expected answer (%q)",
				q.ID, q.Answer)
		}
	}
}

func TestQuestions_AnswerableHaveGradingCriteria(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	for _, q := range c.Questions().Questions {
		if q.MustAbstain {
			continue
		}
		if q.Answer == "" {
			t.Errorf("%s: answerable question has no expected answer", q.ID)
		}
		if len(q.MustContain) == 0 {
			t.Errorf("%s: no grading criteria, so it cannot be scored automatically", q.ID)
		}
	}
}

// Multi-hop questions must actually require multiple hops.
func TestQuestions_MultiHopAreGenuinelyMultiHop(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	by := c.Questions().ByLevel()
	for _, q := range by[L2MultiHop] {
		if q.Hops < 2 {
			t.Errorf("%s claims to be multi-hop but requires %d hop(s)", q.ID, q.Hops)
		}
	}
}

// A temporal question must expect the superseded figure, not the current one.
func TestQuestions_TemporalExpectTheSupersededValue(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	by := c.Questions().ByLevel()
	if len(by[L4Temporal]) == 0 {
		t.Skip("no temporal questions")
	}
	for _, q := range by[L4Temporal] {
		if q.AsOf.IsZero() {
			t.Errorf("%s: temporal question has no as-of date", q.ID)
		}
		if len(q.MustNotContain) == 0 {
			t.Errorf("%s: should forbid the restated figure, or answering with the "+
				"current value would pass", q.ID)
		}
	}
}

// Every answerable question's expected answer must be findable in the corpus,
// or the benchmark is unwinnable by construction.
func TestQuestions_AnswersArePresentInTheCorpus(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	all := allText(c)
	for _, q := range c.Questions().Questions {
		if q.MustAbstain {
			continue
		}
		for _, needle := range q.MustContain {
			if !strings.Contains(all, needle) {
				t.Errorf("%s (%s): required answer fragment %q appears in no document",
					q.ID, q.Level, needle)
			}
		}
	}
}

func TestQuestions_IDsAreUnique(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	seen := map[string]bool{}
	for _, q := range c.Questions().Questions {
		if seen[q.ID] {
			t.Errorf("duplicate question id %q", q.ID)
		}
		seen[q.ID] = true
	}
}

func TestQuestions_EveryItemExplainsItself(t *testing.T) {
	c := Build(Generate(DefaultOptions()), DefaultPerturbations())
	for _, q := range c.Questions().Questions {
		if strings.TrimSpace(q.Why) == "" {
			t.Errorf("%s has no Why; the benchmark report could not explain the result", q.ID)
		}
	}
}

func allText(c *Corpus) string {
	var b strings.Builder
	for _, d := range c.Docs {
		b.WriteString(d.Text)
		b.WriteByte('\n')
	}
	return b.String()
}
