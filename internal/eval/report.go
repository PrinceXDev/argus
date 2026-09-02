package eval

import (
	"fmt"
	"sort"
	"strings"

	"github.com/prince/argus/internal/corpus"
)

// Metrics summarise one arm's performance.
type Metrics struct {
	Arm      string `json:"arm"`
	Describe string `json:"describe"`
	N        int    `json:"n"`

	// Accuracy over answerable questions only. Mixing abstention questions in
	// would let a system that always declines score well on accuracy, which
	// would be nonsense.
	Accuracy          float64 `json:"accuracy"`
	AnswerableN       int     `json:"answerableN"`
	AnswerableCorrect int     `json:"answerableCorrect"`

	// Abstention precision/recall/F1 over the questions where declining is
	// correct. This is the metric no top-k retriever can score on, because it
	// always has a best match to present.
	AbstentionPrecision float64 `json:"abstentionPrecision"`
	AbstentionRecall    float64 `json:"abstentionRecall"`
	AbstentionF1        float64 `json:"abstentionF1"`

	// PoisonASR is the attack success rate: the share of poisoned questions
	// where the false assertion reached the answer. Lower is better.
	PoisonASR float64 `json:"poisonAsr"`
	PoisonN   int     `json:"poisonN"`

	// Attributable precision, reported only for arms that return a derivation.
	Attributable   float64 `json:"attributable"`
	HasDerivations bool    `json:"hasDerivations"`

	// OverAnswering counts answerable-looking responses to unanswerable
	// questions. It is the failure mode that matters most in an investigation
	// and the one a confident RAG system commits silently.
	OverAnswered int `json:"overAnswered"`

	MedianGraphMS int64 `json:"medianGraphMs"`
	MedianTotalMS int64 `json:"medianTotalMs"`
	Errors        int   `json:"errors"`

	// ByLevel is per-level accuracy, which is where the shape of the result
	// lives: a uniform lift looks like tuning, a shape change looks like a
	// different kind of system.
	ByLevel map[corpus.Level]LevelMetrics `json:"byLevel"`
}

// LevelMetrics is one arm's performance on one question class.
type LevelMetrics struct {
	N       int     `json:"n"`
	Correct int     `json:"correct"`
	Score   float64 `json:"score"`
}

// Compute aggregates outcomes into metrics for one arm.
func Compute(arm, describe string, outcomes []Outcome) Metrics {
	m := Metrics{Arm: arm, Describe: describe, N: len(outcomes), ByLevel: map[corpus.Level]LevelMetrics{}}

	var (
		truePos, falsePos, falseNeg int // abstention confusion matrix
		poisonHits                  int
		attrSum                     float64
		attrN                       int
		graphTimes, totalTimes      []int64
	)

	for _, o := range outcomes {
		if o.Err != "" {
			m.Errors++
		}

		lm := m.ByLevel[o.Level]
		lm.N++
		if o.Correct {
			lm.Correct++
		}
		lm.Score = float64(lm.Correct) / float64(lm.N)
		m.ByLevel[o.Level] = lm

		if !o.ShouldAbstain {
			m.AnswerableN++
			if o.Correct {
				m.AnswerableCorrect++
			}
		}

		switch {
		case o.ShouldAbstain && o.Abstained:
			truePos++
		case !o.ShouldAbstain && o.Abstained:
			falsePos++
		case o.ShouldAbstain && !o.Abstained:
			falseNeg++
			m.OverAnswered++
		}

		if o.Level == corpus.L7Poisoned {
			m.PoisonN++
			if o.Poisoned {
				poisonHits++
			}
		}

		if o.HasDerivation {
			attrSum += o.Attributable
			attrN++
		}

		graphTimes = append(graphTimes, o.GraphMS)
		totalTimes = append(totalTimes, o.TotalMS)
	}

	if m.AnswerableN > 0 {
		m.Accuracy = float64(m.AnswerableCorrect) / float64(m.AnswerableN)
	}
	if truePos+falsePos > 0 {
		m.AbstentionPrecision = float64(truePos) / float64(truePos+falsePos)
	}
	if truePos+falseNeg > 0 {
		m.AbstentionRecall = float64(truePos) / float64(truePos+falseNeg)
	}
	if m.AbstentionPrecision+m.AbstentionRecall > 0 {
		m.AbstentionF1 = 2 * m.AbstentionPrecision * m.AbstentionRecall /
			(m.AbstentionPrecision + m.AbstentionRecall)
	}
	if m.PoisonN > 0 {
		m.PoisonASR = float64(poisonHits) / float64(m.PoisonN)
	}
	if attrN > 0 {
		m.Attributable = attrSum / float64(attrN)
		m.HasDerivations = true
	}
	m.MedianGraphMS = median(graphTimes)
	m.MedianTotalMS = median(totalTimes)
	return m
}

func median(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// Report is a full benchmark run.
type Report struct {
	Seed      int64     `json:"seed"`
	Model     string    `json:"model"`
	Questions int       `json:"questions"`
	Metrics   []Metrics `json:"metrics"`
	Outcomes  []Outcome `json:"outcomes,omitempty"`
}

// Markdown renders the report.
//
// The per-level table is the important one, and it is placed before the summary
// deliberately. A uniform lift across every level reads as tuning; a system
// that scores comparably on L1 and separates sharply on L6 and L7 is doing
// something structurally different, and only the per-level view shows that.
func (r Report) Markdown() string {
	var b strings.Builder

	b.WriteString("# ARGUS benchmark results\n\n")
	fmt.Fprintf(&b, "Corpus seed `%d` · %d questions · generation model `%s` · temperature 0\n\n",
		r.Seed, r.Questions, r.Model)
	b.WriteString("Every arm runs against the same FalkorDB instance, the same corpus and the\n" +
		"same model. Differences are attributable to retrieval alone.\n\n")

	b.WriteString("## Accuracy by question class\n\n")
	b.WriteString("Percentage correct. L6 and L7 score a *refusal* as correct.\n\n")

	b.WriteString("| Arm |")
	for _, l := range corpus.AllLevels {
		fmt.Fprintf(&b, " %s |", shortLevel(l))
	}
	b.WriteString("\n|---|")
	for range corpus.AllLevels {
		b.WriteString("---:|")
	}
	b.WriteString("\n")

	for _, m := range r.Metrics {
		fmt.Fprintf(&b, "| %s |", m.Arm)
		for _, l := range corpus.AllLevels {
			lm, ok := m.ByLevel[l]
			if !ok || lm.N == 0 {
				b.WriteString(" — |")
				continue
			}
			fmt.Fprintf(&b, " %.0f%% |", lm.Score*100)
		}
		b.WriteString("\n")
	}

	b.WriteString("\n## Summary\n\n")
	b.WriteString("| Arm | Accuracy | Abstention F1 | Poison ASR | Attributable | Over-answered | Graph p50 | Total p50 |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, m := range r.Metrics {
		attr := "n/a"
		if m.HasDerivations {
			attr = fmt.Sprintf("%.0f%%", m.Attributable*100)
		}
		fmt.Fprintf(&b, "| %s | %.0f%% | %.2f | %.0f%% | %s | %d | %dms | %dms |\n",
			m.Arm, m.Accuracy*100, m.AbstentionF1, m.PoisonASR*100, attr,
			m.OverAnswered, m.MedianGraphMS, m.MedianTotalMS)
	}

	b.WriteString("\n### Reading the columns\n\n")
	b.WriteString("- **Accuracy** is over answerable questions only. Including the abstention\n" +
		"  classes would let a system that always declines score well, which would be\n" +
		"  meaningless.\n")
	b.WriteString("- **Abstention F1** measures whether a system declines exactly when it should.\n" +
		"  Every arm is explicitly told it may say \"Insufficient evidence\", so this is not\n" +
		"  a trick: it measures whether they take the option.\n")
	b.WriteString("- **Poison ASR** is the share of poisoned questions where the false assertion\n" +
		"  reached the answer. Lower is better.\n")
	b.WriteString("- **Attributable** is the share of answer sentences grounded in a claim on the\n" +
		"  returned derivation. It reads `n/a` for passage-based arms because the property\n" +
		"  is undefined without a derivation - not because they scored zero.\n")
	b.WriteString("- **Over-answered** counts confident answers to questions whose evidence was\n" +
		"  deliberately removed. In an investigation this is the failure that matters.\n")

	b.WriteString("\n## Methods\n\n")
	for _, m := range r.Metrics {
		fmt.Fprintf(&b, "- **%s** — %s\n", m.Arm, m.Describe)
	}

	b.WriteString("\n## Limitations\n\n")
	b.WriteString("Read this before citing any number above.\n\n")
	b.WriteString("- **The corpus is synthetic.** It is generated from a ground-truth world, which\n" +
		"  is the only way to score abstention and poisoning honestly, but fiction is not\n" +
		"  your corpus. Real SEC filings are ingested alongside it; the questions here are\n" +
		"  scored against the synthetic layer only.\n")
	b.WriteString("- **Grading is lexical, not semantic.** An answer that is correct but never\n" +
		"  names the expected entity scores zero. The rule is applied identically to every\n" +
		"  arm, so it cannot favour one, but it understates all of them.\n")
	b.WriteString("- **Attributable precision uses word overlap, not entailment.** A strict check\n" +
		"  would need a model, and a model in the grader introduces variance the benchmark\n" +
		"  would then be measuring.\n")
	b.WriteString("- **No LLM judge is used.** That removes judge variance and cost, at the price\n" +
		"  of the strictness noted above.\n")
	b.WriteString("- **GraphRAG-SDK is not included as an arm.** It is Python-only, and ARGUS is a\n" +
		"  Go codebase. Its absence is a real gap in the comparison: it is the strongest\n" +
		"  GraphRAG baseline available and it is not measured here.\n")
	b.WriteString("- **One corpus, one seed, one model.** Results are point-in-time and do not\n" +
		"  generalise to other domains.\n")

	b.WriteString("\nReproduce with `make bench`. The corpus, the questions and this report are\n" +
		"all regenerated from the seed above.\n")

	return b.String()
}

func shortLevel(l corpus.Level) string {
	s := string(l)
	if i := strings.IndexByte(s, '_'); i > 0 {
		return s[:i]
	}
	return s
}
