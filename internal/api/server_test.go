package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prince/argus/internal/answer"
	"github.com/prince/argus/internal/court"
	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/llm"
	"github.com/prince/argus/internal/prove"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

type fakeGraph struct{ rows map[string]kg.Rows }

func (f *fakeGraph) Read(_ context.Context, t kg.Template, _ kg.Params) (kg.Rows, error) {
	return f.rows[t.Name], nil
}
func (f *fakeGraph) ReadGraph(ctx context.Context, _ string, t kg.Template, p kg.Params) (kg.Rows, error) {
	return f.Read(ctx, t, p)
}
func (f *fakeGraph) Plan(context.Context, kg.Template, kg.Params) (string, error) { return "", nil }
func (f *fakeGraph) GraphName() string                                            { return "argus" }
func (f *fakeGraph) Close() error                                                 { return nil }

type fakeLLM struct{ reply string }

func (f *fakeLLM) Complete(context.Context, llm.CompleteRequest) (*llm.CompleteResponse, error) {
	return &llm.CompleteResponse{Text: f.reply, FinishReason: "stop"}, nil
}
func (f *fakeLLM) Model() string { return "fake" }
func (f *fakeLLM) Embed(_ context.Context, in []string) ([][]float64, error) {
	out := make([][]float64, len(in))
	for i := range in {
		out[i] = []float64{0.1, 0.2, 0.3}
	}
	return out, nil
}
func (f *fakeLLM) Dimensions() int { return 3 }

func provenGraph() *fakeGraph {
	return &fakeGraph{rows: map[string]kg.Rows{
		kg.AnchorEntityFulltext.Name: {{"id": "e:acme", "name": "Acme", "type": "Company", "score": 1.0}},
		kg.AnchorEntityVector.Name:   {{"id": "e:acme", "name": "Acme", "type": "Company", "score": 0.9}},
		kg.CandidateClaimsScoped.Name: {{"id": "c9", "text": "Zeta controlled Acme",
			"conf": 0.8, "hops": int64(1), "dist": 0.2}},
		kg.ProveRoots.Name: {{"id": "r1", "text": "root", "conf": 1.0}},
		kg.ProveReach.Name: {{"id": "c9", "weight": int64(223), "cost": int64(2), "hops": int64(2)}},
		kg.ProveChain.Name: {{
			"weight": int64(223), "cost": int64(2), "hops": int64(2),
			"claimIDs":    []any{"r1", "c9"},
			"claimTexts":  []any{"root premise", "Zeta controlled Acme"},
			"stepWeights": []any{int64(223)},
		}},
		kg.ChainEvidence.Name: {{
			"claimID": "r1", "claimText": "root premise", "conf": 1.0,
			"sourceName": "Securities Filing Registry", "sourceKind": "filing",
			"documentTitle": "Schedule 13D", "span": "{\"start\":0,\"end\":10}",
		}},
		kg.GraphStats.Name: {{"nodeCount": int64(1200), "relCount": int64(3400),
			"labelCount": int64(5), "relTypeCount": int64(9)}},
	}}
}

func newServer(g *fakeGraph) *Server {
	f := &fakeLLM{reply: "Zeta controls Acme through a majority holding."}
	return New(Config{
		Graph:    g,
		Prover:   prove.NewEngine(g, f),
		Court:    court.NewEngine(g),
		Writer:   answer.NewWriter(f),
		Embedder: f,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

type sseEvent struct {
	Name string
	Data string
}

// events reads an SSE stream into ordered (name, payload) pairs.
func events(t *testing.T, body io.Reader) []sseEvent {
	t.Helper()
	var out []sseEvent
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var name string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, sseEvent{name, strings.TrimPrefix(line, "data: ")})
		}
	}
	return out
}

func ask(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

// ─── tests ───────────────────────────────────────────────────────────────────

// The proof chain must reach the client before the prose. That ordering is the
// entire visual argument: the reasoning is done in milliseconds and the wait is
// the model talking.
func TestAsk_StreamsVerdictBeforeAnswer(t *testing.T) {
	rec := ask(t, newServer(provenGraph()), `{"question":"Who controls Acme?"}`)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	var order []string
	for _, e := range events(t, rec.Body) {
		order = append(order, e.Name)
	}
	iVerdict, iAnswer := indexOf(order, "verdict"), indexOf(order, "answer")
	if iVerdict < 0 {
		t.Fatalf("no verdict event; got %v", order)
	}
	if iAnswer < 0 {
		t.Fatalf("no answer event; got %v", order)
	}
	if iVerdict > iAnswer {
		t.Errorf("the verdict must stream before the prose; got %v", order)
	}
	if indexOf(order, "done") < 0 {
		t.Errorf("stream did not terminate with done: %v", order)
	}
}

// Nginx buffers proxied responses by default, which would hold the whole stream
// until completion and defeat streaming entirely.
func TestAsk_DisablesProxyBuffering(t *testing.T) {
	rec := ask(t, newServer(provenGraph()), `{"question":"q"}`)
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
}

// The latency split is the "fast" claim, and it must be measurable by the
// client rather than asserted in a README.
func TestAsk_ReportsGraphTimingSeparately(t *testing.T) {
	rec := ask(t, newServer(provenGraph()), `{"question":"q"}`)
	for _, e := range events(t, rec.Body) {
		if e.Name != "done" {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(e.Data), &d); err != nil {
			t.Fatalf("done payload is not JSON: %v", err)
		}
		if _, ok := d["graphMs"]; !ok {
			t.Error("done event must report graph time")
		}
		if _, ok := d["graphOps"]; !ok {
			t.Error("done event must report the graph operation count")
		}
		return
	}
	t.Fatal("no done event")
}

// An abstention must stream as a normal result, not an error.
func TestAsk_AbstentionIsAResultNotAnError(t *testing.T) {
	g := provenGraph()
	g.rows[kg.ProveReach.Name] = nil // nothing derivable
	g.rows[kg.ProveFrontier.Name] = kg.Rows{{"id": "f1", "text": "frontier claim",
		"conf": 0.9, "dist": 0.4, "hops": int64(1)}}

	rec := ask(t, newServer(g), `{"question":"Who owns Ghost Ltd?"}`)
	evs := events(t, rec.Body)
	for _, e := range evs {
		if e.Name == "error" {
			t.Fatalf("abstention surfaced as an error: %s", e.Data)
		}
	}
	var found bool
	for _, e := range evs {
		if e.Name != "answer" {
			continue
		}
		found = true
		var a map[string]any
		if err := json.Unmarshal([]byte(e.Data), &a); err != nil {
			t.Fatalf("answer payload is not JSON: %v", err)
		}
		if a["status"] != string(prove.StatusInsufficient) {
			t.Errorf("status = %v, want %v", a["status"], prove.StatusInsufficient)
		}
		text, _ := a["text"].(string)
		if !strings.Contains(text, "Insufficient evidence") {
			t.Errorf("answer should state the abstention: %q", text)
		}
	}
	if !found {
		t.Fatal("no answer event on abstention")
	}
}

func TestAsk_RejectsEmptyQuestion(t *testing.T) {
	rec := ask(t, newServer(provenGraph()), `{"question":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestAsk_RejectsMalformedBody(t *testing.T) {
	rec := ask(t, newServer(provenGraph()), `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestAsk_RejectsBadAsOf(t *testing.T) {
	rec := ask(t, newServer(provenGraph()), `{"question":"q","asOf":"March 2019"}`)
	// Headers are already sent by then, so the failure arrives as a stream event.
	var sawError bool
	for _, e := range events(t, rec.Body) {
		if e.Name == "error" && strings.Contains(e.Data, "RFC3339") {
			sawError = true
		}
	}
	if !sawError {
		t.Error("a malformed asOf should produce an explanatory error event")
	}
}

// Feature B is expensive; a deployment without it must say so rather than fail
// obscurely.
func TestRetract_DisabledReturnsNotImplemented(t *testing.T) {
	s := newServer(provenGraph()) // no forker configured
	req := httptest.NewRequest(http.MethodPost, "/api/retract",
		strings.NewReader(`{"question":"q"}`))
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not enabled") {
		t.Errorf("response should explain why: %s", rec.Body.String())
	}
}

// The submitted Cypher cannot drift from the running Cypher if the running
// system serves it.
func TestCatalogue_ExposesEveryQuery(t *testing.T) {
	s := newServer(provenGraph())
	req := httptest.NewRequest(http.MethodGet, "/api/catalogue", nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Count   int `json:"count"`
		Queries []struct {
			Name   string `json:"name"`
			Mode   string `json:"mode"`
			Cypher string `json:"cypher"`
			Doc    string `json:"doc"`
		} `json:"queries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Count != len(kg.Catalogue()) || out.Count == 0 {
		t.Fatalf("catalogue reported %d queries, registry has %d", out.Count, len(kg.Catalogue()))
	}
	for _, q := range out.Queries {
		if q.Cypher == "" {
			t.Errorf("%s has no Cypher", q.Name)
		}
		if q.Doc == "" {
			t.Errorf("%s has no documentation", q.Name)
		}
	}
	// The flagship query must be visible to a judge reading this endpoint.
	var sawSPpaths bool
	for _, q := range out.Queries {
		if strings.Contains(q.Cypher, "algo.SPpaths") {
			sawSPpaths = true
		}
	}
	if !sawSPpaths {
		t.Error("the catalogue does not expose the SPpaths proof query")
	}
}

func TestHealth(t *testing.T) {
	s := newServer(provenGraph())
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestStats(t *testing.T) {
	s := newServer(provenGraph())
	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "nodeCount") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestCORSPreflight(t *testing.T) {
	s := newServer(provenGraph())
	req := httptest.NewRequest(http.MethodOptions, "/api/ask", nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Error("preflight did not allow the origin")
	}
}

func TestAsk_BodySizeIsBounded(t *testing.T) {
	huge := bytes.Repeat([]byte("a"), 2<<20)
	body := `{"question":"` + string(huge) + `"}`
	rec := ask(t, newServer(provenGraph()), body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an oversized body should be rejected, got %d", rec.Code)
	}
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}
