// Package api serves the ARGUS HTTP interface.
//
// The ask endpoint streams. That is a product decision, not a technical one:
// the demonstration is that the reasoning finishes in milliseconds and the rest
// of the wait is the language model talking. If the whole response arrived at
// once, that distinction would be invisible and the system would look exactly
// as slow as every other RAG chatbot. Streaming the graph phases as they
// complete makes the proof chain appear before the prose starts, which is the
// entire visual argument.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prince/argus/internal/answer"
	"github.com/prince/argus/internal/court"
	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/llm"
	"github.com/prince/argus/internal/prove"
	"github.com/prince/argus/internal/retract"
)

// Server holds the engines.
//
// The graph field is a kg.Reader, not a Writer, so no request handler can
// mutate the corpus even by mistake. Feature B needs write access to fork, and
// receives it as a separate, explicitly named dependency - which makes the one
// place in the request path that can write visible in the struct definition.
type Server struct {
	graph    kg.Reader
	forker   kg.Writer
	prover   *prove.Engine
	court    *court.Engine
	retract  *retract.Engine
	writer   *answer.Writer
	embedder llm.Embedder
	log      *slog.Logger

	// RetractEnabled gates Feature B. Forking is the most expensive operation
	// in the system, so it is opt-in per deployment.
	RetractEnabled bool

	// Fixture marks a server serving recorded data. It is reported by
	// /api/health so a viewer can always tell which mode produced a result.
	Fixture bool
}

// Config builds a Server.
type Config struct {
	Graph    kg.Reader
	Forker   kg.Writer
	Prover   *prove.Engine
	Court    *court.Engine
	Retract  *retract.Engine
	Writer   *answer.Writer
	Embedder llm.Embedder
	Log      *slog.Logger
}

func New(c Config) *Server {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		graph: c.Graph, forker: c.Forker, prover: c.Prover, court: c.Court,
		retract: c.Retract, writer: c.Writer, embedder: c.Embedder, log: log,
		RetractEnabled: c.Retract != nil && c.Forker != nil,
	}
}

// Routes returns the HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/catalogue", s.handleCatalogue)
	mux.HandleFunc("POST /api/ask", s.handleAsk)
	mux.HandleFunc("POST /api/retract", s.handleRetract)
	return withCORS(withLogging(s.log, mux))
}

// ─── ask ─────────────────────────────────────────────────────────────────────

// AskRequest is the query payload.
type AskRequest struct {
	Question string `json:"question"`
	// LeapBudget is the speculation slider. Zero means the engine default.
	LeapBudget int64 `json:"leapBudget"`
	// MinConfidence is the abstention floor.
	MinConfidence float64 `json:"minConfidence"`
	// AsOf scopes the graph to a point in time, as RFC3339. Empty means now.
	AsOf string `json:"asOf"`
	// Adjudicate runs Feature C. Off by default because maxFlow is O(V*E^2).
	Adjudicate bool `json:"adjudicate"`
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req AskRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if req.Question == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("question is required"))
		return
	}

	stream, err := newSSE(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer stream.close()

	ctx := r.Context()
	if err := s.ask(ctx, stream, req); err != nil {
		// The error goes down the stream rather than as a status code: headers
		// are already sent, and a client mid-stream needs to be told why it
		// stopped.
		stream.send("error", map[string]string{"message": err.Error()})
		s.log.Error("ask failed", "question", req.Question, "err", err)
	}
}

func (s *Server) ask(ctx context.Context, stream *sse, req AskRequest) error {
	q := prove.Question{
		Text:          req.Question,
		LeapBudget:    req.LeapBudget,
		MinConfidence: req.MinConfidence,
	}
	if req.AsOf != "" {
		t, err := time.Parse(time.RFC3339, req.AsOf)
		if err != nil {
			return fmt.Errorf("asOf must be RFC3339: %w", err)
		}
		q.AsOf = t
	}

	// 1. Embed once. The vector is reused by Feature B, so no path in the
	// system re-embeds the same question.
	t0 := time.Now()
	vecs, err := s.embedder.Embed(ctx, []string{req.Question})
	if err != nil {
		return fmt.Errorf("embedding question: %w", err)
	}
	qvec := vecs[0]
	stream.send("phase", map[string]any{"phase": "embedded", "ms": time.Since(t0).Milliseconds()})

	// 2. Prove. This is the whole reasoning step, and the timing sent with it is
	// the point of the latency panel.
	verdict, err := s.prover.AnswerWithVector(ctx, q, qvec)
	if err != nil {
		return err
	}
	stream.send("verdict", verdict)

	// 3. Provenance for the chain.
	var evidence []answer.Evidence
	if len(verdict.Chains) > 0 {
		evidence, err = s.evidence(ctx, verdict)
		if err != nil {
			// Missing provenance weakens the answer but does not invalidate it,
			// and failing the request would hide a correct derivation.
			s.log.Warn("evidence lookup failed", "err", err)
		} else {
			stream.send("evidence", evidence)
		}
	}

	// 4. Adjudicate, if asked.
	var ruling *court.Ruling
	if req.Adjudicate && s.court != nil && len(verdict.Chains) > 0 {
		ruling, err = s.court.Adjudicate(ctx, verdict)
		if err != nil {
			s.log.Warn("adjudication failed", "err", err)
		} else {
			verdict.Status = ruling.Status(verdict.Status)
			stream.send("ruling", ruling)
			// The client stored the verdict emitted before adjudication; resend it
			// so that stored copy reflects the post-adjudication status too.
			stream.send("verdict", verdict)
		}
	}

	// 5. Narrate.
	text, err := s.writer.Write(ctx, answer.Request{
		Question: req.Question,
		Verdict:  verdict,
		Evidence: evidence,
		Ruling:   ruling,
	})
	if err != nil {
		return err
	}
	stream.send("answer", map[string]any{"text": text, "status": verdict.Status})

	stream.send("done", map[string]any{
		"totalMs":  time.Since(t0).Milliseconds(),
		"graphMs":  verdict.Timing.GraphMS,
		"graphOps": verdict.Timing.GraphCalls,
	})
	return nil
}

// evidence resolves each claim on the best chain to its source span.
func (s *Server) evidence(ctx context.Context, v *prove.Verdict) ([]answer.Evidence, error) {
	var ids []string
	seen := map[string]bool{}
	for _, ch := range v.Chains {
		for _, st := range ch.Steps {
			if !seen[st.ClaimID] {
				seen[st.ClaimID] = true
				ids = append(ids, st.ClaimID)
			}
		}
	}
	rows, err := s.graph.Read(ctx, kg.ChainEvidence, kg.Params{"claimIDs": kg.StrParam(ids)})
	if err != nil {
		return nil, err
	}
	out := make([]answer.Evidence, 0, len(rows))
	for _, row := range rows {
		out = append(out, answer.Evidence{
			ClaimID:    row.StrOr("claimID", ""),
			Text:       row.StrOr("claimText", ""),
			Confidence: row.FloatOr("conf", 0),
			SourceName: row.StrOr("sourceName", ""),
			SourceKind: row.StrOr("sourceKind", ""),
			DocTitle:   row.StrOr("documentTitle", ""),
			DocURL:     row.StrOr("documentURL", ""),
			Span:       row.StrOr("span", ""),
		})
	}
	return out, nil
}

// ─── retract ─────────────────────────────────────────────────────────────────

// RetractRequest asks which fact a conclusion rests on.
type RetractRequest struct {
	Question      string  `json:"question"`
	ClaimID       string  `json:"claimId"`
	LeapBudget    int64   `json:"leapBudget"`
	MinConfidence float64 `json:"minConfidence"`
	AsOf          string  `json:"asOf"`
}

func (s *Server) handleRetract(w http.ResponseWriter, r *http.Request) {
	if !s.RetractEnabled {
		writeError(w, http.StatusNotImplemented,
			fmt.Errorf("counterfactual retraction is not enabled on this deployment"))
		return
	}
	var req RetractRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Question == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("question is required"))
		return
	}

	// Forking is expensive and a stalled sweep would hold graph copies in
	// memory, so this path gets its own deadline regardless of the client's.
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	q := prove.Question{
		Text:          req.Question,
		LeapBudget:    req.LeapBudget,
		MinConfidence: req.MinConfidence,
	}
	if req.AsOf != "" {
		t, err := time.Parse(time.RFC3339, req.AsOf)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("asOf must be RFC3339: %w", err))
			return
		}
		q.AsOf = t
	}

	vecs, err := s.embedder.Embed(ctx, []string{req.Question})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	base, err := s.prover.AnswerWithVector(ctx, q, vecs[0])
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(base.Chains) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"question": req.Question,
			"status":   base.Status,
			"message":  "there is no derivation to analyse; ARGUS abstained on this question",
		})
		return
	}

	analysis, err := s.retract.Analyse(ctx, q, vecs[0], base, req.ClaimID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, analysis)
}

// ─── introspection ───────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	type pinger interface{ Ping(context.Context) error }
	status := map[string]any{
		"status":     "ok",
		"retraction": s.RetractEnabled,
		"fixture":    s.Fixture,
	}
	if p, ok := s.graph.(pinger); ok {
		if err := p.Ping(r.Context()); err != nil {
			status["status"] = "degraded"
			status["graph"] = err.Error()
			writeJSON(w, http.StatusServiceUnavailable, status)
			return
		}
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	rows, err := s.graph.Read(r.Context(), kg.GraphStats, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(rows) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, rows[0])
}

// handleCatalogue exposes every Cypher query the product runs.
//
// The hackathon requires submitting "the Cypher queries or graph algorithms the
// product depends on". Serving them live means the submitted queries cannot
// drift from the running ones, and a judge can read the whole query surface
// without cloning anything.
func (s *Server) handleCatalogue(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Name   string   `json:"name"`
		Mode   string   `json:"mode"`
		Doc    string   `json:"doc"`
		Params []string `json:"params,omitempty"`
		Cypher string   `json:"cypher"`
	}
	all := kg.Catalogue()
	out := make([]entry, 0, len(all))
	for _, t := range all {
		out = append(out, entry{
			Name: t.Name, Mode: t.Mode.String(), Doc: t.Doc,
			Params: t.Params, Cypher: t.Cypher,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "queries": out})
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Debug("request", "method", r.Method, "path", r.URL.Path,
			"ms", time.Since(start).Milliseconds())
	})
}

// withCORS allows the Next.js dev server to call the API from another origin.
//
// The allowance is broad because ARGUS serves no credentials and holds no
// per-user state: there is no session for a cross-origin request to ride. A
// deployment that adds authentication must narrow this.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
