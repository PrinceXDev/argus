package ingest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prince/argus/internal/extract"
	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/llm"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

// fakeWriter records every template invocation so tests can assert on the
// shape of what would reach FalkorDB.
type fakeWriter struct {
	mu    sync.Mutex
	calls map[string][]kg.Params
	fail  map[string]error
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{calls: map[string][]kg.Params{}, fail: map[string]error{}}
}

func (f *fakeWriter) record(t kg.Template, p kg.Params) (kg.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[t.Name] = append(f.calls[t.Name], p)
	if err := f.fail[t.Name]; err != nil {
		return nil, err
	}
	return nil, nil
}

func (f *fakeWriter) Write(_ context.Context, t kg.Template, p kg.Params) (kg.Rows, error) {
	return f.record(t, p)
}
func (f *fakeWriter) WriteGraph(_ context.Context, _ string, t kg.Template, p kg.Params) (kg.Rows, error) {
	return f.record(t, p)
}
func (f *fakeWriter) Read(context.Context, kg.Template, kg.Params) (kg.Rows, error) { return nil, nil }
func (f *fakeWriter) ReadGraph(context.Context, string, kg.Template, kg.Params) (kg.Rows, error) {
	return nil, nil
}
func (f *fakeWriter) Plan(context.Context, kg.Template, kg.Params) (string, error) { return "", nil }
func (f *fakeWriter) Raw(context.Context, string, string) (kg.Rows, error)         { return nil, nil }
func (f *fakeWriter) CopyGraph(context.Context, string, string) error              { return nil }
func (f *fakeWriter) DropGraph(context.Context, string) error                      { return nil }
func (f *fakeWriter) ListGraphs(context.Context) ([]string, error)                 { return nil, nil }
func (f *fakeWriter) GraphName() string                                            { return "argus" }
func (f *fakeWriter) Close() error                                                 { return nil }

func (f *fakeWriter) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls[name])
}

func (f *fakeWriter) params(name string) []kg.Params {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]kg.Params(nil), f.calls[name]...)
}

// fakeEmbedder returns a distinct vector per input and counts calls, so tests
// can verify batching.
type fakeEmbedder struct {
	mu    sync.Mutex
	calls int
	seen  []string
}

func (f *fakeEmbedder) Embed(_ context.Context, in []string) ([][]float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seen = append(f.seen, in...)
	out := make([][]float64, len(in))
	for i := range in {
		out[i] = []float64{float64(i), 0.5, 0.25}
	}
	return out, nil
}
func (f *fakeEmbedder) Dimensions() int { return 3 }

// fakeLLM returns a canned extraction for every chunk.
type fakeLLM struct {
	reply string
	err   error
}

func (f *fakeLLM) Complete(_ context.Context, _ llm.CompleteRequest) (*llm.CompleteResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &llm.CompleteResponse{Text: f.reply, Model: "fake", FinishReason: "stop"}, nil
}
func (f *fakeLLM) Model() string { return "fake" }

// ─── fixtures ────────────────────────────────────────────────────────────────

const docText = `Zeta Holdings LLC reported a 60% beneficial ownership stake in Acme Corporation. ` +
	`A holder of more than 50% of voting shares controls the issuer. ` +
	`Dana Reyes signed the filing on behalf of Zeta Holdings LLC.`

func cannedExtraction(t *testing.T) string {
	t.Helper()
	x := extract.Extraction{
		Entities: []extract.Entity{
			{Name: "Zeta Holdings LLC", Type: extract.EntityCompany, Quote: "Zeta Holdings LLC"},
			{Name: "Acme Corporation", Type: extract.EntityCompany, Quote: "Acme Corporation"},
		},
		Claims: []extract.Claim{
			{Ref: "c1", Text: "Zeta Holdings LLC held 60% of Acme Corporation.",
				Quote:    "60% beneficial ownership stake",
				Entities: []string{"Zeta Holdings LLC", "Acme Corporation"}, Confidence: 0.95},
			{Ref: "c2", Text: "A holder above 50% controls the issuer.",
				Quote:    "more than 50% of voting shares controls the issuer",
				Entities: []string{"Acme Corporation"}, Confidence: 0.9},
		},
		Entails: []extract.Entailment{
			{From: "c1", To: "c2", Kind: extract.KindDefinitional, Confidence: 0.95,
				Rationale: "60% exceeds the 50% threshold"},
		},
		Relations: []extract.Relation{
			{From: "Zeta Holdings LLC", To: "Acme Corporation", Type: extract.RelOwns,
				Amount: 60, Quote: "60% beneficial ownership stake"},
		},
	}
	b, err := json.Marshal(x)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTestPipeline(t *testing.T, reply string) (*Pipeline, *fakeWriter, *fakeEmbedder) {
	t.Helper()
	fw := newFakeWriter()
	fe := &fakeEmbedder{}
	p := NewPipeline(
		extract.NewExtractor(&fakeLLM{reply: reply}),
		NewWriter(fw, fe),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return p, fw, fe
}

func rawDoc() RawDocument {
	return RawDocument{
		URL:         "https://www.sec.gov/Archives/example-13d.htm",
		Title:       "Schedule 13D - Acme Corporation",
		Text:        docText,
		PublishedAt: time.Date(2019, 3, 20, 0, 0, 0, 0, time.UTC),
		SHA256:      "abc123",
		Source:      Source{Name: "SEC EDGAR", Kind: "filing", Trust: 0.9},
	}
}

// ─── tests ───────────────────────────────────────────────────────────────────

func TestIngestDocument_WritesFullGraph(t *testing.T) {
	p, fw, _ := newTestPipeline(t, cannedExtraction(t))

	res, err := p.IngestDocument(context.Background(), rawDoc())
	if err != nil {
		t.Fatalf("IngestDocument: %v", err)
	}
	if fw.count(kg.SourceUpsert.Name) != 1 {
		t.Error("source not written")
	}
	if fw.count(kg.DocumentUpsert.Name) != 1 {
		t.Error("document not written")
	}
	if res.Write.Claims == 0 {
		t.Fatal("no claims written")
	}
	if res.Write.Entails == 0 {
		t.Error("no inference edges written; without them no derivation is possible")
	}
	if res.Write.Relations == 0 {
		t.Error("no entity relations written; traversal scoping depends on them")
	}
}

// The proof engine's whole premise is that w_int equals fixed-point
// -ln(confidence). If the writer disagreed with kg.WeightFromConfidence, every
// derivation probability would be silently wrong.
func TestIngestDocument_EntailsWeightMatchesConfidence(t *testing.T) {
	p, fw, _ := newTestPipeline(t, cannedExtraction(t))
	if _, err := p.IngestDocument(context.Background(), rawDoc()); err != nil {
		t.Fatalf("IngestDocument: %v", err)
	}

	calls := fw.params(kg.EntailsUpsert.Name)
	if len(calls) == 0 {
		t.Fatal("no ENTAILS writes")
	}
	for _, c := range calls {
		conf, _ := c["conf"].(float64)
		w, _ := c["wInt"].(int64)
		if want := kg.WeightFromConfidence(conf); w != want {
			t.Errorf("w_int = %d for confidence %.3f, want %d", w, conf, want)
		}
		// A definitional step is capped at 0.95, so 0.95 survives unchanged and
		// the weight must be non-zero: no step is free.
		if w <= 0 {
			t.Errorf("w_int = %d; a non-certain step must cost something", w)
		}
		if leap, _ := c["leap"].(int64); leap <= 0 {
			t.Errorf("leap = %d; every step must consume budget", leap)
		}
	}
}

// Spans must be absolute document offsets, not chunk-relative, or a highlight
// lands in the wrong place once chunk boundaries change.
func TestIngestDocument_SpansAreDocumentAbsolute(t *testing.T) {
	p, fw, _ := newTestPipeline(t, cannedExtraction(t))
	if _, err := p.IngestDocument(context.Background(), rawDoc()); err != nil {
		t.Fatalf("IngestDocument: %v", err)
	}

	for _, c := range fw.params(kg.ClaimUpsert.Name) {
		raw, _ := c["span"].(string)
		var s struct{ Start, End int }
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatalf("span is not valid JSON: %q", raw)
		}
		if s.Start < 0 || s.End > len(docText) || s.Start >= s.End {
			t.Errorf("span %d:%d is not a valid range into the %d-char document",
				s.Start, s.End, len(docText))
		}
		if got := docText[s.Start:s.End]; strings.TrimSpace(got) == "" {
			t.Errorf("span %d:%d resolves to whitespace", s.Start, s.End)
		}
	}
}

// Embedding once per occurrence would multiply the API bill on a real filing,
// where the same entity name recurs in nearly every chunk.
func TestIngestDocument_EmbedsInOneBatchWithoutDuplicates(t *testing.T) {
	p, _, fe := newTestPipeline(t, cannedExtraction(t))
	// Several chunks, so the same entity name recurs and deduplication is
	// actually exercised.
	p.ChunkOptions = ChunkOptions{MaxChars: 90, OverlapSentences: 1}
	// The fake returns the same extraction for every chunk, so quotes are absent
	// from most of them and the quality gate would fire. That gate has its own
	// test; disable it here to isolate batching behaviour.
	p.MaxRejectionRate = 1.0

	if _, err := p.IngestDocument(context.Background(), rawDoc()); err != nil {
		t.Fatalf("IngestDocument: %v", err)
	}
	if fe.calls != 1 {
		t.Errorf("embedder called %d times, want 1 batched call per document", fe.calls)
	}
	seen := map[string]int{}
	for _, s := range fe.seen {
		seen[s]++
	}
	for text, n := range seen {
		if n > 1 {
			t.Errorf("text embedded %d times, want 1: %q", n, text)
		}
	}
}

// Re-ingesting the same document must MERGE onto the same ids, not double the
// graph.
func TestIngestDocument_IsIdempotent(t *testing.T) {
	p, fw, _ := newTestPipeline(t, cannedExtraction(t))
	ctx := context.Background()

	if _, err := p.IngestDocument(ctx, rawDoc()); err != nil {
		t.Fatal(err)
	}
	first := idsOf(fw.params(kg.ClaimUpsert.Name))

	fw2 := newFakeWriter()
	p2 := NewPipeline(
		extract.NewExtractor(&fakeLLM{reply: cannedExtraction(t)}),
		NewWriter(fw2, &fakeEmbedder{}),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if _, err := p2.IngestDocument(ctx, rawDoc()); err != nil {
		t.Fatal(err)
	}
	second := idsOf(fw2.params(kg.ClaimUpsert.Name))

	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("claim counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("claim id changed between identical runs: %s vs %s", first[i], second[i])
		}
	}
}

func idsOf(ps []kg.Params) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		s, _ := p["id"].(string)
		out = append(out, s)
	}
	return out
}

// A collapsed extraction rate means the prompt or chunking is broken. Writing
// the survivors would leave a graph that answers confidently from a fraction of
// the evidence.
func TestIngestDocument_AbortsWhenExtractionQualityCollapses(t *testing.T) {
	// Every quote is fabricated, so every claim fails span integrity.
	bad := extract.Extraction{
		Entities: []extract.Entity{
			{Name: "Zeta Holdings LLC", Type: extract.EntityCompany, Quote: "Zeta Holdings LLC"},
		},
		Claims: []extract.Claim{
			{Ref: "c1", Text: "invented", Quote: "text that is nowhere in the document",
				Entities: []string{"Zeta Holdings LLC"}, Confidence: 0.9},
			{Ref: "c2", Text: "also invented", Quote: "likewise absent from the source",
				Entities: []string{"Zeta Holdings LLC"}, Confidence: 0.9},
		},
	}
	b, _ := json.Marshal(bad)
	p, fw, _ := newTestPipeline(t, string(b))

	_, err := p.IngestDocument(context.Background(), rawDoc())
	if err == nil {
		t.Fatal("expected the run to abort on a collapsed extraction rate")
	}
	if !strings.Contains(err.Error(), "failed validation") {
		t.Errorf("error should name the cause: %v", err)
	}
	if fw.count(kg.ClaimUpsert.Name) != 0 {
		t.Error("no claims should be written when the run aborts")
	}
}

// One malformed chunk should cost that chunk's claims, not the document.
func TestIngestDocument_SurvivesSingleChunkFailure(t *testing.T) {
	p, fw, _ := newTestPipeline(t, "not json at all")
	p.ChunkOptions = ChunkOptions{MaxChars: 90, OverlapSentences: 0}
	p.MaxRejectionRate = 1.0

	res, err := p.IngestDocument(context.Background(), rawDoc())
	if err != nil {
		t.Fatalf("a chunk-level failure must not fail the document: %v", err)
	}
	if res.ChunkErrors == 0 {
		t.Error("chunk errors should be counted")
	}
	// Chunks are still written so their text stays searchable.
	if fw.count(kg.ChunkUpsert.Name) == 0 {
		t.Error("chunks should still be persisted when extraction fails")
	}
}

func TestIngestDocument_HandlesEmptyText(t *testing.T) {
	p, fw, _ := newTestPipeline(t, cannedExtraction(t))
	raw := rawDoc()
	raw.Text = "   "

	res, err := p.IngestDocument(context.Background(), raw)
	if err != nil {
		t.Fatalf("an empty document should be a no-op, not an error: %v", err)
	}
	if res.Write.Chunks != 0 {
		t.Error("no chunks expected")
	}
	// The document node is still created, so a later re-ingest with real text
	// merges onto it.
	if fw.count(kg.DocumentUpsert.Name) != 1 {
		t.Error("document node should still be written")
	}
}

func TestIngestDocument_UsesDocumentDateAsDefaultValidity(t *testing.T) {
	p, fw, _ := newTestPipeline(t, cannedExtraction(t))
	raw := rawDoc()
	if _, err := p.IngestDocument(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	want := raw.PublishedAt.UnixMilli()
	for _, c := range fw.params(kg.ClaimUpsert.Name) {
		got, _ := c["validFrom"].(int64)
		if got != want {
			t.Errorf("validFrom = %d, want the document date %d; "+
				"a claim with no valid_from is invisible to every as-of query", got, want)
		}
		if c["validTo"] != nil {
			t.Errorf("validTo = %v, want nil meaning still current", c["validTo"])
		}
	}
}

func TestParseValidity(t *testing.T) {
	pub := time.Date(2019, 3, 20, 0, 0, 0, 0, time.UTC)

	from, to := parseValidity("", "", pub)
	if from != pub.UnixMilli() || to != nil {
		t.Errorf("empty validity = (%d, %v), want (%d, nil)", from, to, pub.UnixMilli())
	}

	from, to = parseValidity("2018-01-01", "2020-12-31", pub)
	if from != time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("explicit validFrom not honoured: %d", from)
	}
	if to == nil {
		t.Error("explicit validTo not honoured")
	}

	// A malformed date must fall back rather than produce a zero timestamp,
	// which would place the claim in 1970 and make it match every as-of query.
	from, _ = parseValidity("not-a-date", "", pub)
	if from != pub.UnixMilli() {
		t.Errorf("malformed date should fall back to the document date, got %d", from)
	}
}

func TestSummarise(t *testing.T) {
	s := Summarise([]Result{
		{Write: WriteResult{Chunks: 2, Claims: 4, Entails: 1}, ElapsedMS: 100},
		{Write: WriteResult{Chunks: 3, Claims: 5, Entails: 2}, ElapsedMS: 200},
	})
	if s.Documents != 2 || s.Chunks != 5 || s.Claims != 9 || s.Entails != 3 {
		t.Fatalf("bad summary: %+v", s)
	}
	if !strings.Contains(s.String(), "9 claims") {
		t.Errorf("summary string missing counts: %s", s.String())
	}
}

func TestIngestAll_StopsOnError(t *testing.T) {
	p, _, _ := newTestPipeline(t, cannedExtraction(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.IngestAll(ctx, []RawDocument{rawDoc(), rawDoc()})
	if err == nil {
		t.Fatal("a cancelled context must stop the run")
	}
}
