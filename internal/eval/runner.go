package eval

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prince/argus/internal/corpus"
	"github.com/prince/argus/internal/llm"
)

// Runner executes a benchmark across arms.
type Runner struct {
	embedder llm.Embedder
	log      *slog.Logger
	// Concurrency bounds simultaneous questions. Kept low by default: the
	// bottleneck is the model provider's rate limit, and a benchmark that
	// triggers throttling measures the throttle rather than the system.
	Concurrency int
}

func NewRunner(e llm.Embedder, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{embedder: e, log: log, Concurrency: 2}
}

// Run executes every arm over every question and returns the report.
//
// Questions are embedded once, up front, and the vector is shared across all
// arms. That is not only an economy: it guarantees every arm sees an identical
// query representation, so no arm can win by having a better embedding.
func (r *Runner) Run(ctx context.Context, qs corpus.QuestionSet, arms []Arm, model string) (*Report, error) {
	if len(qs.Questions) == 0 {
		return nil, fmt.Errorf("eval: no questions")
	}
	if len(arms) == 0 {
		return nil, fmt.Errorf("eval: no arms")
	}

	texts := make([]string, len(qs.Questions))
	for i, q := range qs.Questions {
		texts[i] = q.Text
	}
	r.log.Info("embedding question set", "questions", len(texts))
	vecs, err := r.embedder.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("eval: embedding questions: %w", err)
	}

	report := &Report{Seed: qs.Seed, Model: model, Questions: len(qs.Questions)}

	for _, arm := range arms {
		start := time.Now()
		r.log.Info("running arm", "arm", arm.Name(), "questions", len(qs.Questions))

		outcomes := make([]Outcome, len(qs.Questions))
		for i, q := range qs.Questions {
			select {
			case <-ctx.Done():
				return report, ctx.Err()
			default:
			}

			query := Query{Text: q.Text, Vec: vecs[i], AsOf: q.AsOf}
			if query.AsOf.IsZero() {
				// Without an explicit as-of, questions are evaluated against the
				// present. Leaving it zero would place every query in year 1 and
				// make the temporal filter exclude everything.
				query.AsOf = time.Now()
			}

			resp, err := arm.Answer(ctx, query)
			outcomes[i] = Grade(q, arm.Name(), resp, err)

			if (i+1)%10 == 0 {
				r.log.Debug("progress", "arm", arm.Name(), "done", i+1, "of", len(qs.Questions))
			}
		}

		m := Compute(arm.Name(), arm.Describe(), outcomes)
		report.Metrics = append(report.Metrics, m)
		report.Outcomes = append(report.Outcomes, outcomes...)

		r.log.Info("arm complete",
			"arm", arm.Name(),
			"accuracy", fmt.Sprintf("%.0f%%", m.Accuracy*100),
			"abstentionF1", fmt.Sprintf("%.2f", m.AbstentionF1),
			"poisonASR", fmt.Sprintf("%.0f%%", m.PoisonASR*100),
			"overAnswered", m.OverAnswered,
			"errors", m.Errors,
			"elapsed", time.Since(start).Round(time.Second))
	}
	return report, nil
}
