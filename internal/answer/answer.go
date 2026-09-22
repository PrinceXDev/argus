// Package answer turns a proof chain into prose.
//
// This is the only place in ARGUS where a language model produces user-facing
// text, and its job is deliberately small: read out a derivation the graph
// already computed. It is never asked what the answer is, only to say what the
// chain says.
//
// The distinction matters for a reason that is easy to state and easy to lose:
// if the model were given the question and a pile of passages, any error it
// made would be unattributable. Given a chain and told to render it, an error
// is immediately visible as a mismatch between the prose and the steps beside
// it on screen. The verifier is the reader, and the chain is what they check
// against.
package answer

import (
	"context"
	"fmt"
	"strings"

	"github.com/prince/argus/internal/court"
	"github.com/prince/argus/internal/llm"
	"github.com/prince/argus/internal/prove"
)

// Evidence is one claim on a chain with its provenance, as returned by
// kg.ChainEvidence.
type Evidence struct {
	ClaimID    string  `json:"claimId"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	SourceName string  `json:"sourceName"`
	SourceKind string  `json:"sourceKind"`
	DocTitle   string  `json:"documentTitle"`
	DocURL     string  `json:"documentUrl"`
	Span       string  `json:"span"`
}

// Request bundles everything the writer may see.
type Request struct {
	Question string
	Verdict  *prove.Verdict
	Evidence []Evidence
	Ruling   *court.Ruling
}

// Writer renders verdicts as prose.
type Writer struct {
	llm       llm.Completer
	MaxTokens int
}

func NewWriter(c llm.Completer) *Writer {
	return &Writer{llm: c, MaxTokens: 700}
}

// Write produces the user-facing answer.
//
// An abstention is rendered without a model call at all. There is nothing to
// summarise, the wording must be exact, and inviting a model to explain why it
// cannot answer is inviting it to speculate about the answer - which is the one
// thing an abstention exists to prevent.
func (w *Writer) Write(ctx context.Context, req Request) (string, error) {
	if req.Verdict == nil {
		return "", fmt.Errorf("answer: no verdict")
	}
	if req.Verdict.Status == prove.StatusInsufficient {
		return abstention(req.Verdict), nil
	}
	if len(req.Verdict.Chains) == 0 {
		return abstention(req.Verdict), nil
	}

	res, err := w.llm.Complete(ctx, llm.CompleteRequest{
		Messages: []llm.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: renderContext(req)},
		},
		Temperature: 0,
		MaxTokens:   w.MaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("answer: %w", err)
	}
	return strings.TrimSpace(res.Text), nil
}

// abstention is written in Go, deterministically.
func abstention(v *prove.Verdict) string {
	var b strings.Builder
	b.WriteString("**Insufficient evidence.** ")
	if v.Reason != "" {
		b.WriteString(capitalise(v.Reason))
		b.WriteString(".")
	} else {
		b.WriteString("No chain of sourced claims supports an answer to this question.")
	}
	if len(v.Frontier) > 0 {
		b.WriteString("\n\nThe evidence reaches this far and stops:\n")
		for _, f := range v.Frontier {
			fmt.Fprintf(&b, "\n- %s _(confidence %.0f%%, %d hop(s) from the anchor)_",
				f.Text, f.Confidence*100, f.Hops)
		}
		b.WriteString("\n\nAn answer would require a document connecting one of these " +
			"to the question. None is present in the corpus.")
	}
	return b.String()
}

// capitalise upper-cases the first letter so a reason composed as a clause
// reads as a sentence when it follows the verdict.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 32
	}
	return string(r)
}

// systemPrompt constrains the writer to the chain.
//
// The instruction not to add outside knowledge is not a hope. Everything the
// model is given is already grounded, the chain is rendered beside the prose,
// and the benchmark's L7 class measures whether ungrounded assertions appear.
// The prompt is one layer; the visible chain is what makes a violation obvious.
const systemPrompt = `You render a completed chain of reasoning into prose for an
investigator. The reasoning has already been performed by a graph database; you are
writing it up, not deciding it.

Rules:

1. State the conclusion in the first sentence, plainly.
2. Then walk the chain in order, one step per sentence or short bullet, naming the
   source for each step.
3. Use ONLY the claims given. Add no fact, inference, caveat or context from your own
   knowledge, however obviously true it seems. If something feels missing, that absence
   is itself the finding.
4. Report the confidence as given. Do not round it up, restate it as certainty, or
   soften it with hedging language of your own.
5. If a claim is marked disputed, say so in the same breath as the claim - never as a
   footnote the reader might skip.
6. If the corroboration analysis says sources share an upstream origin, state that
   explicitly. It is usually the most important thing on the page.
7. No preamble, no "based on the provided context", no offer to help further.

Write in the register of an analyst's note: direct, specific, unhedged about what the
evidence says and unembellished about what it does not.`

// renderContext lays out the chain, its provenance and the ruling as structured
// data rather than prose.
//
// Structured framing is deliberate. Passing the evidence as flowing text would
// let document wording read as instruction; passing it as labelled fields keeps
// the boundary between what the corpus says and what the system asks for.
func renderContext(req Request) string {
	var b strings.Builder

	fmt.Fprintf(&b, "QUESTION\n%s\n\n", req.Question)

	best := req.Verdict.Chains[0]
	fmt.Fprintf(&b, "VERDICT\nstatus: %s\nderivation confidence: %.1f%%\n"+
		"inferential leaps: %d\nchain length: %d hops\nas of: %s\n\n",
		req.Verdict.Status, best.Confidence*100, best.Leaps, best.Hops,
		req.Verdict.AsOf.Format("2 January 2006"))

	evidence := map[string]Evidence{}
	for _, e := range req.Evidence {
		evidence[e.ClaimID] = e
	}
	disputed := map[string][]court.Dispute{}
	if req.Ruling != nil {
		for _, d := range req.Ruling.Disputes {
			disputed[d.ClaimID] = append(disputed[d.ClaimID], d)
		}
	}

	b.WriteString("PROOF CHAIN (in order; each step follows from the one before)\n")
	for i, s := range best.Steps {
		fmt.Fprintf(&b, "\nstep %d\n  claim: %s\n", i+1, s.Text)
		if i > 0 {
			fmt.Fprintf(&b, "  step confidence: %.0f%%\n", s.Conf*100)
		} else {
			b.WriteString("  step confidence: this is the grounded premise\n")
		}
		if e, ok := evidence[s.ClaimID]; ok {
			fmt.Fprintf(&b, "  source: %s (%s)\n  document: %s\n",
				e.SourceName, e.SourceKind, e.DocTitle)
		}
		for _, d := range disputed[s.ClaimID] {
			fmt.Fprintf(&b, "  DISPUTED by %s: %q\n", d.CounterSource.Name, d.CounterText)
		}
	}

	if req.Ruling != nil && len(req.Ruling.Corroboration) > 0 {
		b.WriteString("\nCORROBORATION\n")
		for _, c := range req.Ruling.Corroboration {
			if c.Note == "" {
				continue
			}
			fmt.Fprintf(&b, "  %s\n", c.Note)
		}
	}

	if len(req.Verdict.Chains) > 1 {
		fmt.Fprintf(&b, "\nALTERNATIVE DERIVATIONS: %d other chains reach the same "+
			"conclusion; the strongest is shown above.\n", len(req.Verdict.Chains)-1)
	}

	b.WriteString("\nWrite the analyst's note now.")
	return b.String()
}
