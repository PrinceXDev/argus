package answer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prince/argus/internal/court"
	"github.com/prince/argus/internal/llm"
	"github.com/prince/argus/internal/prove"
)

type fakeLLM struct {
	reply  string
	err    error
	called int
	last   llm.CompleteRequest
}

func (f *fakeLLM) Complete(_ context.Context, r llm.CompleteRequest) (*llm.CompleteResponse, error) {
	f.called++
	f.last = r
	if f.err != nil {
		return nil, f.err
	}
	return &llm.CompleteResponse{Text: f.reply, FinishReason: "stop"}, nil
}
func (f *fakeLLM) Model() string { return "fake" }

func provenVerdict() *prove.Verdict {
	return &prove.Verdict{
		Question: "Which entity ultimately controls Acme Corp?",
		Status:   prove.StatusProven,
		AsOf:     time.Date(2019, 3, 20, 0, 0, 0, 0, time.UTC),
		Chains: []prove.Chain{{
			Confidence: 0.76, Leaps: 2, Hops: 2,
			Steps: []prove.Step{
				{ClaimID: "c1", Text: "Zeta held 60% of Acme", Conf: 1},
				{ClaimID: "c2", Text: "A holder above 50% controls the issuer", Conf: 0.95},
				{ClaimID: "c3", Text: "Zeta controls Acme", Conf: 0.8},
			},
		}},
	}
}

// An abstention must be deterministic prose. Asking a model to explain why it
// cannot answer invites it to speculate about the answer, which is the one
// thing an abstention exists to prevent.
func TestWrite_AbstentionMakesNoModelCall(t *testing.T) {
	f := &fakeLLM{reply: "should never be used"}
	w := NewWriter(f)

	out, err := w.Write(context.Background(), Request{
		Question: "Who owns Ghost Ltd?",
		Verdict: &prove.Verdict{
			Status: prove.StatusInsufficient,
			Reason: "no chain of evidence reaches an answer within 6 inferential leaps",
			Frontier: []prove.FrontierClaim{
				{Text: "Ghost Ltd filed a Form 3 in 2019", Confidence: 0.9, Hops: 1},
			},
		},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if f.called != 0 {
		t.Errorf("the model was called %d times for an abstention", f.called)
	}
	if !strings.Contains(out, "Insufficient evidence") {
		t.Errorf("abstention must say so plainly: %q", out)
	}
	// A refusal must point at the gap, or it is not actionable.
	if !strings.Contains(out, "Ghost Ltd filed a Form 3") {
		t.Errorf("abstention should show the evidence frontier: %q", out)
	}
	if !strings.Contains(out, "reaches this far and stops") {
		t.Errorf("abstention should explain what the frontier is: %q", out)
	}
}

func TestWrite_AbstentionWithoutFrontierStillExplains(t *testing.T) {
	w := NewWriter(&fakeLLM{})
	out, err := w.Write(context.Background(), Request{
		Verdict: &prove.Verdict{Status: prove.StatusInsufficient},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(out, "No chain of sourced claims") {
		t.Errorf("a bare abstention still needs an explanation: %q", out)
	}
}

// The model is given the chain, not the corpus. Everything it can say is
// already grounded.
func TestWrite_ContextContainsOnlyTheChain(t *testing.T) {
	f := &fakeLLM{reply: "Zeta controls Acme."}
	w := NewWriter(f)

	_, err := w.Write(context.Background(), Request{
		Question: "Which entity ultimately controls Acme Corp?",
		Verdict:  provenVerdict(),
		Evidence: []Evidence{
			{ClaimID: "c1", SourceName: "Securities Filing Registry",
				SourceKind: "filing", DocTitle: "Schedule 13D - Acme"},
		},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	ctxText := f.last.Messages[1].Content
	for _, want := range []string{
		"PROOF CHAIN", "Zeta held 60% of Acme", "Securities Filing Registry",
		"derivation confidence: 76.0%",
	} {
		if !strings.Contains(ctxText, want) {
			t.Errorf("context missing %q", want)
		}
	}
	// Determinism matters: the benchmark must not measure sampling noise.
	if f.last.Temperature != 0 {
		t.Errorf("temperature = %v, want 0", f.last.Temperature)
	}
}

// A dispute must reach the writer attached to its step, not as a footnote it
// could render separately and a reader could skip.
func TestWrite_DisputeIsAttachedToItsStep(t *testing.T) {
	f := &fakeLLM{reply: "ok"}
	w := NewWriter(f)

	_, err := w.Write(context.Background(), Request{
		Question: "q",
		Verdict:  provenVerdict(),
		Ruling: &court.Ruling{
			Contested: true,
			Disputes: []court.Dispute{{
				ClaimID:       "c1",
				CounterText:   "Zeta held 71.2% of Acme",
				CounterSource: court.SourceRef{Name: "Market Wire Daily"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	ctxText := f.last.Messages[1].Content

	// The DISPUTED marker must appear inside step 1's block, before step 2.
	step1 := strings.Index(ctxText, "step 1")
	step2 := strings.Index(ctxText, "step 2")
	disputed := strings.Index(ctxText, "DISPUTED")
	if disputed < 0 {
		t.Fatal("the dispute did not reach the writer")
	}
	if !(step1 < disputed && disputed < step2) {
		t.Errorf("the dispute is not attached to the claim it disputes "+
			"(step1=%d disputed=%d step2=%d)", step1, disputed, step2)
	}
}

// The bottleneck finding is usually the most important thing on the page.
func TestWrite_CorroborationNoteReachesTheWriter(t *testing.T) {
	f := &fakeLLM{reply: "ok"}
	w := NewWriter(f)

	note := "6 sources assert this, but they carry only 20% of the independent support"
	_, err := w.Write(context.Background(), Request{
		Question: "q",
		Verdict:  provenVerdict(),
		Ruling: &court.Ruling{
			Corroboration: []court.Corroboration{{ClaimID: "c1", Bottlenecked: true, Note: note}},
		},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(f.last.Messages[1].Content, note) {
		t.Error("the corroboration finding did not reach the writer")
	}
}

func TestWrite_SystemPromptForbidsOutsideKnowledge(t *testing.T) {
	f := &fakeLLM{reply: "ok"}
	w := NewWriter(f)
	if _, err := w.Write(context.Background(), Request{
		Question: "q", Verdict: provenVerdict(),
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sys := f.last.Messages[0].Content
	if !strings.Contains(sys, "Use ONLY the claims given") {
		t.Error("the system prompt must forbid outside knowledge")
	}
	if strings.Contains(sys, "Zeta") {
		t.Error("evidence leaked into the system prompt")
	}
}

func TestWrite_RejectsNilVerdict(t *testing.T) {
	w := NewWriter(&fakeLLM{})
	if _, err := w.Write(context.Background(), Request{}); err == nil {
		t.Fatal("expected an error for a nil verdict")
	}
}

func TestWrite_PropagatesModelError(t *testing.T) {
	w := NewWriter(&fakeLLM{err: llm.ErrRefused})
	if _, err := w.Write(context.Background(), Request{
		Question: "q", Verdict: provenVerdict(),
	}); err == nil {
		t.Fatal("a model failure must surface")
	}
}

func TestWrite_ProvenWithNoChainsFallsBackToAbstention(t *testing.T) {
	f := &fakeLLM{reply: "should not be used"}
	w := NewWriter(f)
	out, err := w.Write(context.Background(), Request{
		Verdict: &prove.Verdict{Status: prove.StatusProven}, // inconsistent: no chains
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if f.called != 0 {
		t.Error("a verdict with no chains must not be narrated")
	}
	if !strings.Contains(out, "Insufficient evidence") {
		t.Errorf("expected an abstention, got %q", out)
	}
}
