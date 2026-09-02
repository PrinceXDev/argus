package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/prince/argus/internal/extract"
	"github.com/prince/argus/internal/kg"
)

// Writer persists a validated extraction into the claim graph.
//
// Write ordering is dictated by FalkorDB's per-graph write serialisation and by
// the MANDATORY constraints. Constraints mean a Claim cannot exist without an
// ASSERTED_BY edge carrying a span, and an ENTAILS edge cannot exist without a
// weight - so the order below is not stylistic, it is the only order in which
// the writes are legal:
//
//	Source -> Document -> Chunk -> Entity -> Claim (+provenance) -> ABOUT
//	-> ENTAILS -> entity relations
//
// Each step is a single atomic query. FalkorDB guarantees a query either
// applies completely or not at all, including any labels it creates, so a
// failure mid-ingest leaves a consistent graph rather than a half-written claim.
type Writer struct {
	graph    kg.Writer
	embedder Embedder
}

// Embedder is the subset of the LLM embedder that ingestion needs.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float64, error)
	Dimensions() int
}

func NewWriter(g kg.Writer, e Embedder) *Writer {
	return &Writer{graph: g, embedder: e}
}

// Source describes a publisher.
type Source struct {
	ID    string
	Name  string
	Kind  string // filing | news | registry | synthetic
	Trust float64
}

// Document describes one ingested document.
type Document struct {
	ID          string
	SourceID    string
	Title       string
	URL         string
	PublishedAt time.Time
	SHA256      string
}

// WriteResult reports what one document's ingest produced.
type WriteResult struct {
	DocumentID string
	Chunks     int
	Entities   int
	Claims     int
	Entails    int
	Relations  int
}

// EnsureSource writes the publisher node.
func (w *Writer) EnsureSource(ctx context.Context, s Source) error {
	if s.Trust <= 0 {
		// A neutral prior. Real trust is computed by PageRank over the citation
		// network rather than assigned here, so this is only a starting value
		// for a source nothing has cited yet.
		s.Trust = 0.5
	}
	_, err := w.graph.Write(ctx, kg.SourceUpsert, kg.Params{
		"id": s.ID, "name": s.Name, "kind": s.Kind, "trust": s.Trust,
	})
	return err
}

// EnsureDocument writes the document node and links it to its source.
func (w *Writer) EnsureDocument(ctx context.Context, d Document) error {
	_, err := w.graph.Write(ctx, kg.DocumentUpsert, kg.Params{
		"id":          d.ID,
		"sourceID":    d.SourceID,
		"title":       d.Title,
		"url":         d.URL,
		"publishedAt": d.PublishedAt.UnixMilli(),
		"sha256":      d.SHA256,
	})
	return err
}

// ChunkPayload pairs a chunk with the extraction taken from it.
type ChunkPayload struct {
	Chunk     TextChunk
	Extracted extract.Validated
}

// WriteDocument persists every chunk and its extraction.
//
// Embeddings for the whole document are computed in one batched call before any
// write. Doing it up front means the (slow, rate-limited, failure-prone) network
// work happens outside the write path, so a provider hiccup aborts before
// touching the graph rather than half-way through it.
func (w *Writer) WriteDocument(ctx context.Context, d Document, payloads []ChunkPayload) (WriteResult, error) {
	res := WriteResult{DocumentID: d.ID}
	if len(payloads) == 0 {
		return res, nil
	}

	texts, index := collectEmbeddable(d.ID, payloads)
	vectors, err := w.embedder.Embed(ctx, texts)
	if err != nil {
		return res, fmt.Errorf("ingest: embedding document %s: %w", d.ID, err)
	}
	if len(vectors) != len(texts) {
		return res, fmt.Errorf("ingest: embedder returned %d vectors for %d inputs",
			len(vectors), len(texts))
	}
	vecFor := func(key string) []any {
		i, ok := index[key]
		if !ok {
			return nil
		}
		return kg.VecParam(vectors[i])
	}

	for _, p := range payloads {
		chunkID := ChunkID(d.ID, p.Chunk.Index)

		if _, err := w.graph.Write(ctx, kg.ChunkUpsert, kg.Params{
			"id": chunkID, "documentID": d.ID,
			"text":  p.Chunk.Text,
			"index": int64(p.Chunk.Index),
			// Offsets are stored so the UI can highlight evidence inside the
			// original document, not just inside the chunk.
			"startChar": int64(p.Chunk.StartChar),
			"endChar":   int64(p.Chunk.EndChar),
			"vec":       vecFor("chunk:" + chunkID),
		}); err != nil {
			return res, fmt.Errorf("ingest: chunk %s: %w", chunkID, err)
		}
		res.Chunks++

		// --- entities ---
		entityIDs := map[string]string{}
		for _, e := range p.Extracted.Entities {
			id := EntityID(string(e.Type), e.Name)
			entityIDs[e.Name] = id
			if _, err := w.graph.Write(ctx, kg.EntityUpsert, kg.Params{
				"id": id, "name": e.Name, "type": string(e.Type),
				"vec": vecFor("entity:" + id),
			}); err != nil {
				return res, fmt.Errorf("ingest: entity %s: %w", id, err)
			}
			res.Entities++
		}

		// --- claims ---
		claimIDs := map[string]string{}
		for _, c := range p.Extracted.Claims {
			// Spans are recorded relative to the whole document, not the chunk,
			// so a highlight still resolves after chunk boundaries change.
			absStart := p.Chunk.StartChar + c.Span.Start
			absEnd := p.Chunk.StartChar + c.Span.End
			id := ClaimID(d.ID, absStart, absEnd, c.Text)
			claimIDs[c.Ref] = id

			span, err := json.Marshal(map[string]int{"start": absStart, "end": absEnd})
			if err != nil {
				return res, err
			}
			validFrom, validTo := parseValidity(c.ValidFrom, c.ValidTo, d.PublishedAt)

			if _, err := w.graph.Write(ctx, kg.ClaimUpsert, kg.Params{
				"id": id, "text": c.Text, "conf": c.Confidence,
				"validFrom": validFrom, "validTo": validTo,
				"vec":        vecFor("claim:" + id),
				"documentID": d.ID, "chunkID": chunkID,
				"span": string(span),
			}); err != nil {
				return res, fmt.Errorf("ingest: claim %s: %w", id, err)
			}
			res.Claims++

			for _, name := range c.Entities {
				eid, ok := entityIDs[name]
				if !ok {
					continue
				}
				if _, err := w.graph.Write(ctx, kg.ClaimAbout, kg.Params{
					"claimID": id, "entityID": eid,
				}); err != nil {
					return res, fmt.Errorf("ingest: claim.about %s: %w", id, err)
				}
			}
		}

		// --- inference edges ---
		for _, e := range p.Extracted.Entails {
			from, okF := claimIDs[e.From]
			to, okT := claimIDs[e.To]
			if !okF || !okT {
				continue
			}
			// kg.WeightFromConfidence is the single definition of how confidence
			// becomes distance. Computing it here, in Go, rather than in Cypher
			// keeps it unit-testable and keeps one source of truth.
			if _, err := w.graph.Write(ctx, kg.EntailsUpsert, kg.Params{
				"fromID": from, "toID": to,
				"wInt":      kg.WeightFromConfidence(e.Confidence),
				"leap":      e.Leap,
				"conf":      e.Confidence,
				"rationale": e.Rationale,
			}); err != nil {
				return res, fmt.Errorf("ingest: entails %s->%s: %w", from, to, err)
			}
			res.Entails++
		}

		// --- entity relations ---
		for _, r := range p.Extracted.Relations {
			from, okF := entityIDs[r.From]
			to, okT := entityIDs[r.To]
			if !okF || !okT {
				continue
			}
			tpl, params := relationTemplate(r, from, to, d)
			if _, err := w.graph.Write(ctx, tpl, params); err != nil {
				return res, fmt.Errorf("ingest: relation %s->%s: %w", from, to, err)
			}
			res.Relations++
		}
	}
	return res, nil
}

// relationTemplate maps a relation type to its template.
//
// The relationship type is baked into each template rather than passed as a
// parameter, because a parameterised relationship type would require building
// the query as a string - reintroducing exactly the injection surface the
// template registry exists to eliminate.
func relationTemplate(r extract.ValidRelation, from, to string, d Document) (kg.Template, kg.Params) {
	base := kg.Params{
		"fromID": from, "toID": to,
		"at":         d.PublishedAt.UnixMilli(),
		"documentID": d.ID,
	}
	switch r.Type {
	case extract.RelOwns:
		base["pct"] = r.Amount
		return kg.EntityOwns, base
	case extract.RelControls:
		base["role"] = ""
		return kg.EntityControls, base
	default:
		base["amount"] = r.Amount
		return kg.EntityTransacted, base
	}
}

// collectEmbeddable gathers every text that needs a vector - chunks, claims and
// entity names - deduplicated and keyed by the graph id it will be written to.
//
// Three things are embedded because three things are searched. Chunk vectors
// serve passage retrieval; claim vectors are what CandidateClaimsScoped ranks
// with vec.cosineDistance inside a traversal-scoped set; entity vectors are the
// semantic anchor for questions that describe a party without naming it.
//
// Deduplication is not just an optimisation. The same entity name recurs in
// nearly every chunk of a filing, and embedding it once per occurrence would
// multiply the API bill by an order of magnitude on a real document.
func collectEmbeddable(docID string, payloads []ChunkPayload) ([]string, map[string]int) {
	var texts []string
	index := map[string]int{}
	add := func(key, text string) {
		if text == "" {
			return
		}
		if _, seen := index[key]; seen {
			return
		}
		index[key] = len(texts)
		texts = append(texts, text)
	}
	for _, p := range payloads {
		add("chunk:"+ChunkID(docID, p.Chunk.Index), p.Chunk.Text)

		for _, e := range p.Extracted.Entities {
			add("entity:"+EntityID(string(e.Type), e.Name), e.Name)
		}
		for _, c := range p.Extracted.Claims {
			absStart := p.Chunk.StartChar + c.Span.Start
			absEnd := p.Chunk.StartChar + c.Span.End
			add("claim:"+ClaimID(docID, absStart, absEnd, c.Text), c.Text)
		}
	}
	return texts, index
}

// parseValidity resolves a claim's validity window.
//
// A claim with no stated start date inherits the document's publication date:
// a filing asserts something as of when it was filed, and leaving valid_from
// unset would make the claim invisible to every as-of query. An unset valid_to
// means "still current", which is what allows a temporal question to be
// answered rather than guessed.
func parseValidity(from, to string, published time.Time) (int64, any) {
	start := published.UnixMilli()
	if from != "" {
		if t, err := time.Parse("2006-01-02", from); err == nil {
			start = t.UnixMilli()
		}
	}
	if to != "" {
		if t, err := time.Parse("2006-01-02", to); err == nil {
			return start, t.UnixMilli()
		}
	}
	return start, nil
}
