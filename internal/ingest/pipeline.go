package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/prince/argus/internal/extract"
)

// RawDocument is a document as it arrives from a fetcher, before chunking.
type RawDocument struct {
	// URL is the canonical location, and the basis of the document id.
	URL         string
	Title       string
	Text        string
	PublishedAt time.Time
	SHA256      string
	Source      Source
}

// Pipeline runs the full ingest: chunk, extract, embed, write.
type Pipeline struct {
	extractor *extract.Extractor
	writer    *Writer
	log       *slog.Logger

	// ChunkOptions controls splitting.
	ChunkOptions ChunkOptions
	// Concurrency bounds simultaneous extraction calls. This is a rate-limit
	// dial for the LLM provider, not a database one: extraction is the slow,
	// expensive, failure-prone stage, and writes are serialised per graph
	// downstream regardless of how wide this is.
	Concurrency int
	// MaxRejectionRate aborts the run when extraction quality collapses.
	// Silently ingesting a corpus where most claims fail span integrity would
	// produce a thin graph that looks fine and answers badly, so this fails
	// loudly instead.
	MaxRejectionRate float64
}

func NewPipeline(e *extract.Extractor, w *Writer, log *slog.Logger) *Pipeline {
	if log == nil {
		log = slog.Default()
	}
	return &Pipeline{
		extractor:        e,
		writer:           w,
		log:              log,
		ChunkOptions:     DefaultChunkOptions(),
		Concurrency:      4,
		MaxRejectionRate: 0.5,
	}
}

// Result summarises one document's ingest.
type Result struct {
	Document    Document
	Write       WriteResult
	Extraction  extract.Report
	ElapsedMS   int64
	ChunkErrors int
}

// IngestDocument runs the whole pipeline for one document.
func (p *Pipeline) IngestDocument(ctx context.Context, raw RawDocument) (Result, error) {
	start := time.Now()
	var res Result

	if raw.Source.ID == "" {
		raw.Source.ID = SourceID(raw.Source.Name)
	}
	doc := Document{
		ID:          DocumentID(raw.URL),
		SourceID:    raw.Source.ID,
		Title:       raw.Title,
		URL:         raw.URL,
		PublishedAt: raw.PublishedAt,
		SHA256:      raw.SHA256,
	}
	res.Document = doc

	if err := p.writer.EnsureSource(ctx, raw.Source); err != nil {
		return res, fmt.Errorf("ingest: source: %w", err)
	}
	if err := p.writer.EnsureDocument(ctx, doc); err != nil {
		return res, fmt.Errorf("ingest: document: %w", err)
	}

	chunks := Chunk(raw.Text, p.ChunkOptions)
	if len(chunks) == 0 {
		p.log.Warn("document produced no chunks", "document", doc.ID, "title", doc.Title)
		return res, nil
	}

	payloads, report, chunkErrs := p.extractAll(ctx, doc, chunks)
	res.Extraction = report
	res.ChunkErrors = chunkErrs

	// A collapsed extraction rate means the prompt, the model or the chunking is
	// broken. Writing the survivors anyway would leave a graph that answers
	// confidently from a fraction of the evidence.
	if rate := report.RejectionRate(); rate > p.MaxRejectionRate {
		return res, fmt.Errorf(
			"ingest: %s: %.0f%% of extracted claims failed validation (limit %.0f%%); "+
				"refusing to write a partial graph",
			doc.Title, rate*100, p.MaxRejectionRate*100)
	}

	wr, err := p.writer.WriteDocument(ctx, doc, payloads)
	if err != nil {
		return res, err
	}
	res.Write = wr
	res.ElapsedMS = time.Since(start).Milliseconds()

	p.log.Info("ingested",
		"document", doc.Title,
		"chunks", wr.Chunks,
		"claims", wr.Claims,
		"entails", wr.Entails,
		"rejected", len(report.Rejections),
		"ms", res.ElapsedMS)
	return res, nil
}

// extractAll runs extraction across chunks concurrently and reassembles the
// results in document order.
//
// Order matters: chunk index determines claim ids and the order the UI walks
// evidence in, so results are sorted back into position rather than appended as
// goroutines finish.
//
// A chunk that fails extraction is counted and skipped, not fatal. One
// malformed page in a filing should cost that page's claims, not the document -
// and the aggregate rejection-rate check above still catches systemic failure.
func (p *Pipeline) extractAll(ctx context.Context, doc Document, chunks []TextChunk) ([]ChunkPayload, extract.Report, int) {
	type slot struct {
		payload ChunkPayload
		report  extract.Report
		ok      bool
	}
	slots := make([]slot, len(chunks))

	sem := make(chan struct{}, max(1, p.Concurrency))
	var wg sync.WaitGroup
	var mu sync.Mutex
	errCount := 0

	for i, ch := range chunks {
		wg.Add(1)
		go func(i int, ch TextChunk) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			v, rep, err := p.extractor.ExtractChunk(ctx, extract.Chunk{
				ID:         ChunkID(doc.ID, ch.Index),
				DocumentID: doc.ID,
				Text:       ch.Text,
				DocTitle:   doc.Title,
				DocDate:    doc.PublishedAt.Format("2006-01-02"),
			})
			if err != nil {
				mu.Lock()
				errCount++
				mu.Unlock()
				p.log.Warn("chunk extraction failed",
					"document", doc.Title, "chunk", ch.Index, "err", err)
				// The chunk is still written, without claims, so its text stays
				// searchable and the document's structure is complete.
				slots[i] = slot{payload: ChunkPayload{Chunk: ch}, ok: true}
				return
			}
			slots[i] = slot{payload: ChunkPayload{Chunk: ch, Extracted: v}, report: rep, ok: true}
		}(i, ch)
	}
	wg.Wait()

	var payloads []ChunkPayload
	var report extract.Report
	for _, s := range slots {
		if !s.ok {
			continue
		}
		payloads = append(payloads, s.payload)
		report.Merge(s.report)
	}
	sort.Slice(payloads, func(i, j int) bool {
		return payloads[i].Chunk.Index < payloads[j].Chunk.Index
	})
	return payloads, report, errCount
}

// IngestAll runs the pipeline over many documents sequentially.
//
// Documents are processed one at a time on purpose. Concurrency lives inside a
// document, across its chunks, where the work is LLM-bound. Running whole
// documents in parallel would multiply write pressure on a graph that
// serialises writes anyway, and would make the progress log unreadable.
func (p *Pipeline) IngestAll(ctx context.Context, docs []RawDocument) ([]Result, error) {
	results := make([]Result, 0, len(docs))
	for i, d := range docs {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}
		r, err := p.IngestDocument(ctx, d)
		if err != nil {
			return results, fmt.Errorf("document %d/%d (%s): %w", i+1, len(docs), d.Title, err)
		}
		results = append(results, r)
	}
	return results, nil
}

// Summary aggregates results for the ingest report.
type Summary struct {
	Documents int
	Chunks    int
	Entities  int
	Claims    int
	Entails   int
	Relations int
	Rejected  int
	ElapsedMS int64
}

func Summarise(results []Result) Summary {
	var s Summary
	for _, r := range results {
		s.Documents++
		s.Chunks += r.Write.Chunks
		s.Entities += r.Write.Entities
		s.Claims += r.Write.Claims
		s.Entails += r.Write.Entails
		s.Relations += r.Write.Relations
		s.Rejected += len(r.Extraction.Rejections)
		s.ElapsedMS += r.ElapsedMS
	}
	return s
}

func (s Summary) String() string {
	return fmt.Sprintf(
		"%d documents, %d chunks, %d entities, %d claims, %d inference edges, "+
			"%d relations, %d rejected, %.1fs",
		s.Documents, s.Chunks, s.Entities, s.Claims, s.Entails,
		s.Relations, s.Rejected, float64(s.ElapsedMS)/1000)
}
