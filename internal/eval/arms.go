// Package eval is the ARGUS benchmark harness.
//
// The design rule is that every number in the report must be produced by this
// code from a real run. Nothing is filled in by hand, and the harness ships in
// the repository so a judge can re-run it.
//
// Five arms compete on identical terms: the same graph, the same corpus, the
// same generation model, the same temperature, the same grader. Any difference
// in score is therefore attributable to retrieval, which is the only claim the
// benchmark is entitled to make.
//
// Running the baselines against FalkorDB rather than a separate vector store is
// deliberate. It removes the obvious objection - that ARGUS wins because its
// datastore is faster - by making the datastore identical across arms. What
// differs is what is asked of it.
package eval

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prince/argus/internal/answer"
	"github.com/prince/argus/internal/court"
	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/llm"
	"github.com/prince/argus/internal/prove"
)

// Arm is one retrieval system under test.
type Arm interface {
	// Name identifies the arm in the report.
	Name() string
	// Describe is the one-line method summary printed in the report, so a reader
	// can see what was actually compared.
	Describe() string
	// Answer produces a response and whether the system declined to answer.
	Answer(ctx context.Context, q Query) (Response, error)
}

// Query is one benchmark item, already embedded.
type Query struct {
	Text string
	Vec  []float64
	AsOf time.Time
}

// Response is an arm's output.
type Response struct {
	Text string
	// Abstained is true when the system declined to answer. Only a system that
	// can decline will ever set this, which is the point of measuring it.
	Abstained bool
	// Claims are the atomic assertions the answer is built from, when the arm
	// can enumerate them. Used for attributable precision, which a
	// passage-based arm cannot report because it has no derivation.
	Claims []string
	// GraphMS and TotalMS separate database work from model work.
	GraphMS int64
	TotalMS int64
}

// ─── A0: keyword ─────────────────────────────────────────────────────────────

type keywordArm struct {
	graph kg.Reader
	llm   llm.Completer
	TopK  int64
}

func NewKeywordArm(g kg.Reader, l llm.Completer) Arm {
	return &keywordArm{graph: g, llm: l, TopK: 8}
}

func (a *keywordArm) Name() string { return "A0-keyword" }
func (a *keywordArm) Describe() string {
	return "BM25-style full-text retrieval over chunk text, top-8 passages into the model."
}

func (a *keywordArm) Answer(ctx context.Context, q Query) (Response, error) {
	start := time.Now()
	t0 := time.Now()
	rows, err := a.graph.Read(ctx, kg.BaselineKeyword, kg.Params{
		"q": sanitiseFulltext(q.Text), "limit": a.TopK,
	})
	graphMS := time.Since(t0).Milliseconds()
	if err != nil {
		return Response{}, err
	}
	text, err := answerFromPassages(ctx, a.llm, q.Text, passages(rows))
	return Response{
		Text: text, GraphMS: graphMS, TotalMS: time.Since(start).Milliseconds(),
	}, err
}

// ─── A1: vector ──────────────────────────────────────────────────────────────

type vectorArm struct {
	graph kg.Reader
	llm   llm.Completer
	TopK  int64
}

func NewVectorArm(g kg.Reader, l llm.Completer) Arm {
	return &vectorArm{graph: g, llm: l, TopK: 8}
}

func (a *vectorArm) Name() string { return "A1-vector" }
func (a *vectorArm) Describe() string {
	return "Pure vector RAG: HNSW nearest neighbours over chunk embeddings, top-8 passages."
}

func (a *vectorArm) Answer(ctx context.Context, q Query) (Response, error) {
	start := time.Now()
	t0 := time.Now()
	rows, err := a.graph.Read(ctx, kg.BaselineVector, kg.Params{
		"k": a.TopK, "qvec": kg.VecParam(q.Vec),
	})
	graphMS := time.Since(t0).Milliseconds()
	if err != nil {
		return Response{}, err
	}
	text, err := answerFromPassages(ctx, a.llm, q.Text, passages(rows))
	return Response{
		Text: text, GraphMS: graphMS, TotalMS: time.Since(start).Milliseconds(),
	}, err
}

// ─── A2: hybrid ──────────────────────────────────────────────────────────────

type hybridArm struct {
	graph kg.Reader
	llm   llm.Completer
	TopK  int64
}

func NewHybridArm(g kg.Reader, l llm.Completer) Arm {
	return &hybridArm{graph: g, llm: l, TopK: 8}
}

func (a *hybridArm) Name() string { return "A2-hybrid" }
func (a *hybridArm) Describe() string {
	return "Hybrid retrieval: keyword and vector results fused by reciprocal rank fusion."
}

func (a *hybridArm) Answer(ctx context.Context, q Query) (Response, error) {
	start := time.Now()
	t0 := time.Now()

	kw, err := a.graph.Read(ctx, kg.BaselineKeyword, kg.Params{
		"q": sanitiseFulltext(q.Text), "limit": a.TopK,
	})
	if err != nil {
		return Response{}, err
	}
	vec, err := a.graph.Read(ctx, kg.BaselineVector, kg.Params{
		"k": a.TopK, "qvec": kg.VecParam(q.Vec),
	})
	if err != nil {
		return Response{}, err
	}
	graphMS := time.Since(t0).Milliseconds()

	fused := reciprocalRankFusion([]kg.Rows{kw, vec}, int(a.TopK))
	text, err := answerFromPassages(ctx, a.llm, q.Text, fused)
	return Response{
		Text: text, GraphMS: graphMS, TotalMS: time.Since(start).Milliseconds(),
	}, err
}

// reciprocalRankFusion merges ranked lists by summing 1/(k+rank).
//
// RRF is used rather than score normalisation because full-text scores and
// cosine distances are on incomparable scales, and normalising them would bake
// an arbitrary weighting into a baseline that is supposed to be neutral. RRF
// only needs the ordering.
func reciprocalRankFusion(lists []kg.Rows, limit int) []passage {
	const k = 60.0 // the constant from the original RRF paper
	score := map[string]float64{}
	seen := map[string]passage{}

	for _, list := range lists {
		for rank, row := range list {
			id, ok := row.Str("chunkId")
			if !ok {
				continue
			}
			score[id] += 1.0 / (k + float64(rank+1))
			if _, dup := seen[id]; !dup {
				seen[id] = passage{
					Text:  row.StrOr("text", ""),
					Title: row.StrOr("documentTitle", ""),
				}
			}
		}
	}

	type scored struct {
		id string
		s  float64
	}
	ranked := make([]scored, 0, len(score))
	for id, s := range score {
		ranked = append(ranked, scored{id, s})
	}
	// Ties broken by id so the fusion is deterministic across runs.
	for i := 1; i < len(ranked); i++ {
		for j := i; j > 0; j-- {
			a, b := ranked[j-1], ranked[j]
			if b.s > a.s || (b.s == a.s && b.id < a.id) {
				ranked[j-1], ranked[j] = b, a
				continue
			}
			break
		}
	}
	out := make([]passage, 0, limit)
	for i := 0; i < len(ranked) && i < limit; i++ {
		out = append(out, seen[ranked[i].id])
	}
	return out
}

// ─── A3: naive GraphRAG ──────────────────────────────────────────────────────

type graphNaiveArm struct {
	graph kg.Reader
	llm   llm.Completer
	TopK  int64
}

func NewGraphNaiveArm(g kg.Reader, l llm.Completer) Arm {
	return &graphNaiveArm{graph: g, llm: l, TopK: 10}
}

func (a *graphNaiveArm) Name() string { return "A3-graph-naive" }
func (a *graphNaiveArm) Describe() string {
	return "Naive GraphRAG: entity match, one-hop expansion, source chunks. " +
		"Returns a neighbourhood, not a derivation."
}

func (a *graphNaiveArm) Answer(ctx context.Context, q Query) (Response, error) {
	start := time.Now()
	t0 := time.Now()
	rows, err := a.graph.Read(ctx, kg.BaselineGraphNaive, kg.Params{
		"q": sanitiseFulltext(q.Text), "limit": a.TopK,
	})
	graphMS := time.Since(t0).Milliseconds()
	if err != nil {
		return Response{}, err
	}
	text, err := answerFromPassages(ctx, a.llm, q.Text, passages(rows))
	return Response{
		Text: text, GraphMS: graphMS, TotalMS: time.Since(start).Milliseconds(),
	}, err
}

// ─── A5: ARGUS ───────────────────────────────────────────────────────────────

type argusArm struct {
	prover *prove.Engine
	court  *court.Engine
	writer *answer.Writer
	graph  kg.Reader
	// Adjudicate runs Feature C per question. Off by default in the benchmark:
	// maxFlow is O(V*E^2) and it changes wording rather than correctness, so
	// including it would inflate latency without changing the comparison.
	Adjudicate bool
}

func NewArgusArm(p *prove.Engine, c *court.Engine, w *answer.Writer, g kg.Reader) Arm {
	return &argusArm{prover: p, court: c, writer: w, graph: g}
}

func (a *argusArm) Name() string { return "A5-argus" }
func (a *argusArm) Describe() string {
	return "ARGUS: traversal-scoped candidates, cost-bounded shortest-path derivation " +
		"over -ln(confidence), calibrated abstention."
}

func (a *argusArm) Answer(ctx context.Context, q Query) (Response, error) {
	start := time.Now()
	question := prove.Question{Text: q.Text, AsOf: q.AsOf}

	v, err := a.prover.AnswerWithVector(ctx, question, q.Vec)
	if err != nil {
		return Response{}, err
	}

	var ruling *court.Ruling
	if a.Adjudicate && a.court != nil && len(v.Chains) > 0 {
		ruling, _ = a.court.Adjudicate(ctx, v)
		if ruling != nil {
			v.Status = ruling.Status(v.Status)
		}
	}

	text, err := a.writer.Write(ctx, answer.Request{
		Question: q.Text, Verdict: v, Ruling: ruling,
	})
	if err != nil {
		return Response{}, err
	}

	// The claims on the winning chain are what makes attributable precision
	// measurable: every assertion in the answer should correspond to one of
	// these. No passage-based arm can produce this list.
	var claims []string
	if best := v.Best(); best != nil {
		for _, s := range best.Steps {
			claims = append(claims, s.Text)
		}
	}

	return Response{
		Text:      text,
		Abstained: v.Status == prove.StatusInsufficient,
		Claims:    claims,
		GraphMS:   v.Timing.GraphMS,
		TotalMS:   time.Since(start).Milliseconds(),
	}, nil
}

// ─── shared ──────────────────────────────────────────────────────────────────

type passage struct {
	Text  string
	Title string
}

func passages(rows kg.Rows) []passage {
	out := make([]passage, 0, len(rows))
	for _, r := range rows {
		out = append(out, passage{
			Text:  r.StrOr("text", ""),
			Title: r.StrOr("documentTitle", ""),
		})
	}
	return out
}

// baselinePrompt is shared by every passage-based arm.
//
// It is written to give the baselines their best shot: they are explicitly
// permitted to say the context is insufficient. Without that permission the
// abstention comparison would be rigged - a system never told it may decline
// obviously will not. Whether they take the option is exactly what L6 measures.
const baselinePrompt = `You answer questions using only the provided context passages.

Answer directly and concisely. Do not speculate beyond the passages.

If the passages do not contain enough information to answer, say
"Insufficient evidence" and explain what is missing. It is better to say that than
to guess.`

func answerFromPassages(ctx context.Context, c llm.Completer, question string, ps []passage) (string, error) {
	var b strings.Builder
	b.WriteString("CONTEXT PASSAGES\n")
	if len(ps) == 0 {
		b.WriteString("(none retrieved)\n")
	}
	for i, p := range ps {
		fmt.Fprintf(&b, "\n[%d] from %s\n%s\n", i+1, p.Title, p.Text)
	}
	fmt.Fprintf(&b, "\nQUESTION\n%s\n", question)

	res, err := c.Complete(ctx, llm.CompleteRequest{
		Messages: []llm.Message{
			{Role: "system", Content: baselinePrompt},
			{Role: "user", Content: b.String()},
		},
		Temperature: 0,
		MaxTokens:   600,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Text), nil
}

// sanitiseFulltext strips RediSearch query operators so a question containing a
// hyphen or bracket searches for those words instead of failing to parse.
func sanitiseFulltext(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '@', '!', '{', '}', '(', ')', '[', ']', '|', '-', '~', '*', '"', ':', '\'', '\\', '/', '>', '<', '=':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
