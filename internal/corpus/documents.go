package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// Doc is a rendered synthetic document.
type Doc struct {
	ID          string
	URL         string
	Title       string
	Text        string
	PublishedAt time.Time
	SourceName  string
	SourceKind  string // filing | news | reference | pressrelease
	SourceTrust float64
	// Tags mark documents the perturbations touched, so the benchmark can
	// explain a result rather than just score it.
	Tags []string
}

func (d Doc) SHA256() string {
	sum := sha256.Sum256([]byte(d.Text))
	return hex.EncodeToString(sum[:])
}

// Perturbations control the four deliberate corruptions that make the hard
// benchmark classes scoreable.
type Perturbations struct {
	// Withhold is how many ownership links have their evidence deleted. The
	// resulting questions are provably unanswerable, which is the only way to
	// score abstention honestly.
	Withhold int
	// Poison is how many false claims to inject from a low-trust source.
	Poison int
	// Contradict is how many facts get a second, conflicting document.
	Contradict int
	// Restate is how many facts get superseded by a later filing, so an as-of
	// query must return the earlier value.
	Restate int
}

func DefaultPerturbations() Perturbations {
	return Perturbations{Withhold: 6, Poison: 8, Contradict: 8, Restate: 8}
}

// Corpus is the generated document set plus the record of what was perturbed.
type Corpus struct {
	World *World
	Docs  []Doc

	// Withheld records ownership links whose evidence was deliberately removed.
	Withheld []Holding
	// Poisoned records the false claims injected, with the truth they contradict.
	Poisoned []Poison
	// Contradicted records facts that have two conflicting sources.
	Contradicted []Contradiction
	// Restated records facts superseded by a later filing.
	Restated []Restatement
}

type Poison struct {
	Company string
	// FalseText is the assertion injected into the corpus.
	FalseText string
	// Truth is what the world actually says, for scoring.
	Truth string
	// SourceName is the low-trust publisher that carried it.
	SourceName string
}

type Contradiction struct {
	Issuer       string
	TruePercent  float64
	FalsePercent float64
	FalseSource  string
}

type Restatement struct {
	Issuer     string
	OldPercent float64
	NewPercent float64
	AsOf       time.Time
	RestatedAt time.Time
}

// Build renders a world into documents, applying the perturbations.
func Build(w *World, p Perturbations) *Corpus {
	rng := rand.New(rand.NewSource(w.Seed + 1))
	c := &Corpus{World: w}

	// The definitional document. Every ownership chain needs it: without the
	// rule "a holder above 50% controls the issuer", a percentage is just a
	// number and no control conclusion is derivable. Making it a separate
	// document is deliberate - it forces genuine multi-document reasoning
	// rather than letting one passage answer the question.
	c.Docs = append(c.Docs, Doc{
		Title:       "Beneficial Ownership Reporting Guide",
		URL:         "synthetic://reference/ownership-guide",
		SourceName:  "Regulatory Reference Manual",
		SourceKind:  "reference",
		SourceTrust: 0.95,
		PublishedAt: time.Date(w.seedYear(), 1, 5, 0, 0, 0, 0, time.UTC),
		Text: strings.TrimSpace(`
Beneficial Ownership Reporting Guide, Section 4: Control Determinations.

A holder of more than 50 percent of the voting shares of an issuer controls that issuer.
Control so established persists until the holding falls to 50 percent or below.

Where control is established through an intermediate entity, the ultimate controlling
party is the entity at the top of the unbroken chain of majority holdings. Each link in
such a chain must independently exceed 50 percent for control to pass through it.
`),
	})

	// Choose which links to withhold before rendering, so their filings are
	// simply never produced.
	//
	// Selection walks the holdings with a stride rather than taking a prefix, so
	// the gaps land in different chains instead of gutting the first one. It
	// keeps scanning until the target count is met - an earlier version used a
	// fixed index and silently produced fewer perturbations than requested,
	// which quietly thinned the hardest benchmark levels.
	withheld := map[string]bool{}
	for _, h := range pick(w.Holdings, p.Withhold, 7, func(h Holding) bool { return true }) {
		withheld[h.Holder+"|"+h.Issuer] = true
		c.Withheld = append(c.Withheld, h)
	}

	// --- ownership filings ----------------------------------------------------
	for i, h := range w.Holdings {
		if withheld[h.Holder+"|"+h.Issuer] {
			continue
		}
		c.Docs = append(c.Docs, ownershipFiling(h, i))
	}

	// --- officer filings ------------------------------------------------------
	for i, o := range w.Officers {
		c.Docs = append(c.Docs, officerFiling(o, i))
	}

	// --- ring transfers -------------------------------------------------------
	for i, t := range w.Transfers {
		c.Docs = append(c.Docs, transferReport(t, i))
	}

	// --- registry extract for addresses --------------------------------------
	if len(w.Ring) > 0 {
		c.Docs = append(c.Docs, registryExtract(w))
	}

	// --- perturbation: contradictions ----------------------------------------
	seenContra := map[string]bool{}
	contraTargets := pick(w.Holdings, p.Contradict, 5, func(h Holding) bool {
		if withheld[h.Holder+"|"+h.Issuer] || seenContra[h.Issuer] {
			return false
		}
		seenContra[h.Issuer] = true
		return true
	})
	for i, h := range contraTargets {
		// A different percentage, still above 50 so the control conclusion is
		// unchanged - the dispute is about the figure, not the outcome. That is
		// the realistic case, and the harder one to detect.
		false_ := round1(51 + rng.Float64()*20)
		if false_ == h.Percent {
			false_ = round1(false_ + 3)
		}
		src := "Market Wire Daily"
		c.Docs = append(c.Docs, contradictingReport(h, false_, src, i))
		c.Contradicted = append(c.Contradicted, Contradiction{
			Issuer: h.Issuer, TruePercent: h.Percent, FalsePercent: false_, FalseSource: src,
		})
	}

	// --- perturbation: temporal restatements ---------------------------------
	seenRestate := map[string]bool{}
	restateTargets := pick(w.Holdings, p.Restate, 3, func(h Holding) bool {
		if withheld[h.Holder+"|"+h.Issuer] || seenRestate[h.Issuer] {
			return false
		}
		seenRestate[h.Issuer] = true
		return true
	})
	for i, h := range restateTargets {
		newPct := round1(51 + rng.Float64()*45)
		if newPct == h.Percent {
			newPct = round1(newPct + 2)
		}
		restatedAt := h.From.AddDate(2, 0, 0)
		c.Docs = append(c.Docs, amendedFiling(h, newPct, restatedAt, i))
		c.Restated = append(c.Restated, Restatement{
			Issuer: h.Issuer, OldPercent: h.Percent, NewPercent: newPct,
			AsOf: h.From.AddDate(0, 6, 0), RestatedAt: restatedAt,
		})
	}

	// --- perturbation: poisoning ---------------------------------------------
	// Injected from a deliberately obscure, uncorroborated publisher. This is
	// the case §2b of the threat model describes: span integrity cannot block
	// it, because the sentence really is in the corpus. What must limit it is
	// source trust and corroboration, and the benchmark measures whether it does.
	seenPoison := map[string]bool{}
	poisonTargets := pick(w.Companies, p.Poison, 11, func(co Company) bool {
		if seenPoison[co.Name] {
			return false
		}
		seenPoison[co.Name] = true
		return true
	})
	for i, co := range poisonTargets {
		falseText := fmt.Sprintf(
			"%s is under active criminal investigation for securities fraud.", co.Name)
		src := "Anonymous Finance Blog"
		c.Docs = append(c.Docs, poisonPost(co, falseText, src, i))
		c.Poisoned = append(c.Poisoned, Poison{
			Company:   co.Name,
			FalseText: falseText,
			Truth: fmt.Sprintf(
				"No document in the ground-truth world states any investigation of %s.", co.Name),
			SourceName: src,
		})
	}

	return c
}

func (w *World) seedYear() int {
	if len(w.Holdings) > 0 {
		return w.Holdings[0].From.Year()
	}
	return 2017
}

// ─── document templates ──────────────────────────────────────────────────────
//
// Each template states one fact plainly and surrounds it with the register of a
// real filing. The surrounding prose is not padding: a corpus of bare facts is
// trivially easy for any retriever, and would make every arm score alike.

func ownershipFiling(h Holding, i int) Doc {
	return Doc{
		Title:      fmt.Sprintf("Schedule 13D - %s", h.Issuer),
		URL:        fmt.Sprintf("synthetic://filings/13d-%d", i),
		SourceName: "Securities Filing Registry", SourceKind: "filing", SourceTrust: 0.95,
		PublishedAt: h.From,
		Text: strings.TrimSpace(fmt.Sprintf(`
SCHEDULE 13D
Statement of Beneficial Ownership

Issuer: %s
Reporting Person: %s
Date of Event Requiring Filing: %s

Item 1. Security and Issuer.
This statement relates to the common stock of %s.

Item 5. Interest in Securities of the Issuer.
%s reports beneficial ownership of %.1f percent of the outstanding voting shares of %s
as of %s. The shares were acquired in a series of privately negotiated transactions.

Item 6. Contracts, Arrangements or Understandings.
Other than as described in this statement, the reporting person has no contracts or
arrangements with respect to the securities of the issuer.
`,
			h.Issuer, h.Holder, h.From.Format("January 2, 2006"),
			h.Issuer,
			h.Holder, h.Percent, h.Issuer, h.From.Format("January 2, 2006"))),
	}
}

func officerFiling(o Officer, i int) Doc {
	return Doc{
		Title:      fmt.Sprintf("Form 3 - %s", o.Company),
		URL:        fmt.Sprintf("synthetic://filings/form3-%d", i),
		SourceName: "Securities Filing Registry", SourceKind: "filing", SourceTrust: 0.95,
		PublishedAt: o.From,
		Text: strings.TrimSpace(fmt.Sprintf(`
FORM 3
Initial Statement of Beneficial Ownership of Securities

Issuer: %s
Name of Reporting Person: %s
Date of Event Requiring Statement: %s

Relationship of Reporting Person to Issuer:
%s serves as %s of %s.

The reporting person signed this statement in the capacity described above.
`,
			o.Company, o.Person, o.From.Format("January 2, 2006"),
			o.Person, o.Role, o.Company)),
	}
}

func transferReport(t Transfer, i int) Doc {
	return Doc{
		Title:      fmt.Sprintf("Payment advice %d", i+1000),
		URL:        fmt.Sprintf("synthetic://payments/advice-%d", i),
		SourceName: "Correspondent Bank Advice", SourceKind: "filing", SourceTrust: 0.85,
		PublishedAt: t.At,
		Text: strings.TrimSpace(fmt.Sprintf(`
PAYMENT ADVICE

Value date: %s
Ordering customer: %s
Beneficiary customer: %s
Amount: USD %.2f

Remittance information: intercompany settlement, no further details provided.
This advice is issued for reconciliation purposes and does not constitute a statement
of account.
`, t.At.Format("2006-01-02"), t.From, t.To, t.Amount)),
	}
}

// registryExtract is what makes the fraud ring discoverable. It states each
// company's registered address as a plain fact; the ring itself - a closed loop
// of transfers between companies sharing one address - is never described
// anywhere, and exists only as graph structure.
func registryExtract(w *World) Doc {
	var b strings.Builder
	b.WriteString("COMPANIES REGISTRY - EXTRACT OF REGISTERED OFFICES\n\n")
	b.WriteString("The following entities are recorded at the registered offices shown.\n\n")
	for _, name := range w.Ring {
		if co, ok := w.CompanyByName(name); ok {
			fmt.Fprintf(&b, "%s is registered at %s in the jurisdiction of %s.\n",
				co.Name, co.Address, co.Jurisdiction)
		}
	}
	b.WriteString("\nThis extract is provided without warranty as to completeness.\n")
	return Doc{
		Title:      "Companies Registry extract",
		URL:        "synthetic://registry/extract-1",
		SourceName: "Companies Registry", SourceKind: "filing", SourceTrust: 0.9,
		PublishedAt: time.Date(w.seedYear()+2, 6, 1, 0, 0, 0, 0, time.UTC),
		Text:        b.String(),
		Tags:        []string{"ring"},
	}
}

func contradictingReport(h Holding, falsePct float64, source string, i int) Doc {
	return Doc{
		Title:      fmt.Sprintf("Stake in %s reported at %.1f percent", h.Issuer, falsePct),
		URL:        fmt.Sprintf("synthetic://news/contradiction-%d", i),
		SourceName: source, SourceKind: "news", SourceTrust: 0.4,
		PublishedAt: h.From.AddDate(0, 1, 0),
		Tags:        []string{"contradiction"},
		Text: strings.TrimSpace(fmt.Sprintf(`
%s

According to people familiar with the matter, %s holds %.1f percent of the voting shares
of %s. The figure differs from the position stated in the issuer's regulatory filings.

The reporting person did not respond to a request for comment. This publication has not
independently verified the shareholding.
`, h.Issuer, h.Holder, falsePct, h.Issuer)),
	}
}

func amendedFiling(h Holding, newPct float64, at time.Time, i int) Doc {
	return Doc{
		Title:      fmt.Sprintf("Schedule 13D/A - %s (Amendment)", h.Issuer),
		URL:        fmt.Sprintf("synthetic://filings/13da-%d", i),
		SourceName: "Securities Filing Registry", SourceKind: "filing", SourceTrust: 0.95,
		PublishedAt: at,
		Tags:        []string{"restatement"},
		Text: strings.TrimSpace(fmt.Sprintf(`
SCHEDULE 13D/A
Amendment No. 1

Issuer: %s
Reporting Person: %s
Date of Event Requiring Filing: %s

Item 5. Interest in Securities of the Issuer, as amended.
As of %s, %s reports beneficial ownership of %.1f percent of the outstanding voting
shares of %s. This amendment reflects subsequent open-market acquisitions and supersedes
the holding previously reported for this issuer.

The position reported in the original statement remains accurate as of the date of that
statement.
`,
			h.Issuer, h.Holder, at.Format("January 2, 2006"),
			at.Format("January 2, 2006"), h.Holder, newPct, h.Issuer)),
	}
}

func poisonPost(co Company, falseText, source string, i int) Doc {
	return Doc{
		Title:      fmt.Sprintf("What they are not telling you about %s", co.Name),
		URL:        fmt.Sprintf("synthetic://blog/post-%d", i),
		SourceName: source, SourceKind: "pressrelease", SourceTrust: 0.1,
		PublishedAt: time.Date(2021, 9, 1, 0, 0, 0, 0, time.UTC),
		Tags:        []string{"poison"},
		Text: strings.TrimSpace(fmt.Sprintf(`
%s

Our sources indicate serious irregularities at this company. %s

We understand the matter is at an advanced stage. No filings have been made public and
the company has issued no statement. Readers should draw their own conclusions.
`, co.Name, falseText)),
	}
}

// pick selects up to n items from xs, walking with a stride so selections are
// spread across the list rather than clustered at the front, and skipping items
// the predicate rejects.
//
// It scans the whole list if it has to. A fixed-index scheme reaches the target
// only when nothing collides, which silently under-populates the hardest
// benchmark levels and makes the headline numbers rest on a handful of items.
func pick[T any](xs []T, n, stride int, ok func(T) bool) []T {
	if n <= 0 || len(xs) == 0 {
		return nil
	}
	if stride < 1 {
		stride = 1
	}
	out := make([]T, 0, n)
	seen := make([]bool, len(xs))
	for step := 0; step < len(xs) && len(out) < n; step++ {
		i := (step * stride) % len(xs)
		// The stride may revisit an index before covering the list; fall back to
		// linear scanning for anything it skipped.
		if seen[i] {
			for j := 0; j < len(xs); j++ {
				if !seen[j] {
					i = j
					break
				}
			}
			if seen[i] {
				break
			}
		}
		seen[i] = true
		if ok(xs[i]) {
			out = append(out, xs[i])
		}
	}
	return out
}
