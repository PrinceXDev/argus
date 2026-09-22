package api

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/llm"
)

// Fixture mode.
//
// A recorded graph and a recorded model, so the full request path can run with
// no database and no API key. It exists for three reasons:
//
//   - The frontend can be developed and reviewed without infrastructure.
//   - A demo recording does not depend on a live provider staying up.
//   - Every layer above kg.Reader is exercised for real. The proof engine, the
//     evidence lookup, the answer writer, the SSE framing and the abstention
//     logic all run their genuine code paths; only the two external
//     dependencies are recorded.
//
// It is not a shortcut past the real system and must never be presented as one:
// argus-api prints a warning on startup and the API reports fixture:true on
// /api/health so a viewer can always tell which mode produced a result.

// FixtureGraph is a recorded FalkorDB.
//
// The recorded world is a three-hop ownership chain: Meridian Holdings LLC
// holds 62% of Cobalt Capital Partners LP, which holds 55% of Ashcroft Freight
// Corp. Composed with the >50% control rule from a separate reference document,
// that entails ultimate control - a conclusion no single passage contains.
type FixtureGraph struct {
	// scope carries the question currently being answered.
	//
	// The graph needs it because only one of the two anchor templates receives
	// the question text as a parameter - the vector anchor gets a vector - and a
	// fixture that keyed off parameters alone would let a question anchor
	// through the path that cannot see it. Sharing scope with the embedder,
	// which does see every question, keeps the two consistent.
	scope *Scope
}

// Scope records the question in flight so the fixture graph and the fixture
// embedder agree on which recorded world to serve.
//
// It is a single slot guarded by a mutex, which means fixture mode answers one
// question at a time. That is the correct trade for a demo and development
// aid; it is stated here rather than discovered under concurrent load.
type Scope struct {
	mu       sync.Mutex
	question string
}

func (s *Scope) set(q string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.question = q
}

func (s *Scope) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.question
}

// NewFixture builds a matched graph and model sharing one scope.
func NewFixture(dim int) (*FixtureGraph, *FixtureLLM) {
	sc := &Scope{}
	return &FixtureGraph{scope: sc}, &FixtureLLM{dim: dimOr(dim), scope: sc}
}

func dimOr(d int) int {
	if d <= 0 {
		return 8
	}
	return d
}

// abstains reports whether the question in flight has no supporting evidence in
// the recorded world.
func (f *FixtureGraph) abstains() bool {
	if f.scope == nil {
		return false
	}
	return strings.Contains(strings.ToLower(f.scope.get()), "ghost")
}

func (f *FixtureGraph) GraphName() string { return "argus-fixture" }
func (f *FixtureGraph) Close() error      { return nil }
func (f *FixtureGraph) Plan(context.Context, kg.Template, kg.Params) (string, error) {
	return "ProcedureCall | algo.SPpaths", nil
}

func (f *FixtureGraph) Read(ctx context.Context, t kg.Template, p kg.Params) (kg.Rows, error) {
	return f.ReadGraph(ctx, f.GraphName(), t, p)
}

func (f *FixtureGraph) ReadGraph(_ context.Context, _ string, t kg.Template, p kg.Params) (kg.Rows, error) {
	// A question about Ghost Nominees Ltd has no supporting filing in the
	// recorded world, which drives the abstention path end to end rather than
	// short-circuiting it.
	abstain := f.abstains()

	switch t.Name {
	case kg.GraphStats.Name:
		return kg.Rows{{
			"nodeCount": int64(4187), "relCount": int64(11342),
			"labelCount": int64(6), "relTypeCount": int64(11),
		}}, nil

	case kg.AnchorEntityFulltext.Name:
		if abstain {
			// The entity is in the registry; what is missing is any filing
			// stating who controls it. Anchoring succeeds and the derivation
			// fails, which is the realistic shape of an unanswerable question
			// and the one that produces a useful frontier.
			return kg.Rows{
				{"id": "e:company:ghost-nominees-ltd", "name": "Ghost Nominees Ltd",
					"type": "Company", "score": 5.9},
			}, nil
		}
		return kg.Rows{
			{"id": "e:company:ashcroft-freight-corp", "name": "Ashcroft Freight Corp",
				"type": "Company", "score": 6.4},
		}, nil

	case kg.AnchorEntityVector.Name:
		if abstain {
			return kg.Rows{
				{"id": "e:company:ghost-nominees-ltd", "name": "Ghost Nominees Ltd",
					"type": "Company", "score": 0.88},
			}, nil
		}
		return kg.Rows{
			{"id": "e:company:ashcroft-freight-corp", "name": "Ashcroft Freight Corp",
				"type": "Company", "score": 0.91},
			{"id": "e:company:cobalt-capital-partners-lp", "name": "Cobalt Capital Partners LP",
				"type": "Company", "score": 0.74},
		}, nil

	case kg.CandidateClaimsScoped.Name:
		if abstain {
			// A plausible answer exists as a candidate - nothing derives it.
			return kg.Rows{
				{"id": "c:ghost-control", "text": "Ghost Nominees Ltd is controlled by an undisclosed party.",
					"conf": 0.4, "hops": int64(1), "dist": 0.33},
			}, nil
		}
		return kg.Rows{
			{"id": "c:ultimate", "text": "Meridian Holdings LLC ultimately controls Ashcroft Freight Corp.",
				"conf": 0.86, "hops": int64(2), "dist": 0.18},
		}, nil

	case kg.ProveRoots.Name:
		if abstain {
			return kg.Rows{
				{"id": "c:f1", "text": "Ghost Nominees Ltd is registered at 44 Pembroke Street.",
					"conf": 0.94},
			}, nil
		}
		return kg.Rows{
			{"id": "c:root", "text": "Meridian Holdings LLC reported beneficial ownership of 62.0 percent of the outstanding voting shares of Cobalt Capital Partners LP.",
				"conf": 0.98},
		}, nil

	case kg.ProveReach.Name:
		if abstain {
			return nil, nil
		}
		// 22 + 51 + 105 = 178 milli-nats -> exp(-0.178) ~= 0.837
		return kg.Rows{
			{"id": "c:ultimate", "weight": int64(178), "cost": int64(3), "hops": int64(3)},
		}, nil

	case kg.ProveChain.Name:
		if abstain {
			return nil, nil
		}
		return kg.Rows{{
			"weight": int64(178), "cost": int64(3), "hops": int64(3),
			"claimIDs": []any{"c:root", "c:rule", "c:mid", "c:ultimate"},
			"claimTexts": []any{
				"Meridian Holdings LLC reported beneficial ownership of 62.0 percent of the outstanding voting shares of Cobalt Capital Partners LP.",
				"A holder of more than 50 percent of the voting shares of an issuer controls that issuer.",
				"Cobalt Capital Partners LP reported beneficial ownership of 55.0 percent of the outstanding voting shares of Ashcroft Freight Corp.",
				"Meridian Holdings LLC ultimately controls Ashcroft Freight Corp.",
			},
			"stepWeights": []any{int64(22), int64(51), int64(105)},
		}}, nil

	case kg.ProveFrontier.Name:
		return kg.Rows{
			{"id": "c:f1", "text": "Ghost Nominees Ltd is registered at 44 Pembroke Street.",
				"conf": 0.94, "dist": 0.42, "hops": int64(1)},
			{"id": "c:f2", "text": "Ghost Nominees Ltd filed a Form 3 on 12 May 2019.",
				"conf": 0.9, "dist": 0.51, "hops": int64(1)},
		}, nil

	case kg.ChainEvidence.Name:
		return kg.Rows{
			{"claimID": "c:root", "claimText": "Meridian Holdings LLC reported beneficial ownership of 62.0 percent…",
				"conf": 0.98, "sourceName": "Securities Filing Registry", "sourceKind": "filing",
				"documentTitle": "Schedule 13D — Cobalt Capital Partners LP",
				"documentURL":   "synthetic://filings/13d-4", "sourceTrust": 0.95,
				"span": "{\"start\":210,\"end\":298}"},
			{"claimID": "c:rule", "claimText": "A holder of more than 50 percent…",
				"conf": 0.95, "sourceName": "Regulatory Reference Manual", "sourceKind": "reference",
				"documentTitle": "Beneficial Ownership Reporting Guide",
				"documentURL":   "synthetic://reference/ownership-guide", "sourceTrust": 0.95,
				"span": "{\"start\":88,\"end\":176}"},
			{"claimID": "c:mid", "claimText": "Cobalt Capital Partners LP reported beneficial ownership of 55.0 percent…",
				"conf": 0.98, "sourceName": "Securities Filing Registry", "sourceKind": "filing",
				"documentTitle": "Schedule 13D — Ashcroft Freight Corp",
				"documentURL":   "synthetic://filings/13d-9", "sourceTrust": 0.95,
				"span": "{\"start\":198,\"end\":284}"},
			{"claimID": "c:ultimate", "claimText": "Meridian Holdings LLC ultimately controls Ashcroft Freight Corp.",
				"conf": 0.86, "sourceName": "Securities Filing Registry", "sourceKind": "filing",
				"documentTitle": "Schedule 13D — Ashcroft Freight Corp",
				"documentURL":   "synthetic://filings/13d-9", "sourceTrust": 0.95,
				"span": "{\"start\":198,\"end\":284}"},
		}, nil

	case kg.CourtContradictions.Name:
		return kg.Rows{{
			"claimID": "c:mid", "counterID": "c:wire",
			"counterText": "Cobalt Capital Partners LP holds 71.4 percent of the voting shares of Ashcroft Freight Corp.",
			"counterConf": 0.55, "detectedBy": "nli",
			"sourceID": "s:market-wire-daily", "sourceName": "Market Wire Daily",
			"sourceTrust": 0.4,
		}}, nil

	case kg.CourtDirectSupport.Name:
		id, _ := p["claimID"].(string)
		if id != "c:mid" {
			return kg.Rows{
				{"id": "s:securities-filing-registry", "name": "Securities Filing Registry",
					"kind": "filing", "trust": 0.95},
			}, nil
		}
		// Six outlets assert this - but they all republish one press release.
		rows := kg.Rows{
			{"id": "s:securities-filing-registry", "name": "Securities Filing Registry",
				"kind": "filing", "trust": 0.95},
		}
		for i := 0; i < 5; i++ {
			rows = append(rows, kg.Row{
				"id":   fmt.Sprintf("s:outlet-%d", i),
				"name": fmt.Sprintf("Regional Business Wire %d", i+1),
				"kind": "news", "trust": 0.45,
			})
		}
		return rows, nil

	case kg.CourtCorroboration.Name:
		id, _ := p["claimID"].(string)
		if id != "c:mid" {
			return kg.Rows{{"flow": int64(95), "originCount": int64(1)}}, nil
		}
		// Naive expectation would be 95 + 5*45 = 320. Max flow says 140,
		// because the five outlets bottleneck through one origin.
		return kg.Rows{{"flow": int64(140), "originCount": int64(2)}}, nil

	case kg.CourtSourceTrust.Name:
		return kg.Rows{
			{"id": "s:securities-filing-registry", "name": "Securities Filing Registry",
				"kind": "filing", "score": 0.41},
			{"id": "s:companies-registry", "name": "Companies Registry",
				"kind": "filing", "score": 0.27},
			{"id": "s:market-wire-daily", "name": "Market Wire Daily",
				"kind": "news", "score": 0.09},
		}, nil
	}
	return nil, nil
}

// FixtureLLM is a recorded model. It embeds deterministically and narrates the
// recorded chain.
type FixtureLLM struct {
	dim   int
	scope *Scope
}

// NewFixtureLLM builds a standalone recorded model. Prefer NewFixture, which
// pairs a model with a graph that can see the same question.
func NewFixtureLLM(dim int) *FixtureLLM {
	return &FixtureLLM{dim: dimOr(dim), scope: &Scope{}}
}

func (f *FixtureLLM) Model() string   { return "fixture" }
func (f *FixtureLLM) Dimensions() int { return f.dim }

// Embed hashes the input into a stable pseudo-vector. Values are meaningless;
// what matters is that the same text always produces the same vector, so a
// fixture run is reproducible.
func (f *FixtureLLM) Embed(_ context.Context, inputs []string) ([][]float64, error) {
	// The first input of a request is the question. Recording it is what lets
	// the graph serve the matching recorded world.
	if len(inputs) > 0 && f.scope != nil {
		f.scope.set(inputs[0])
	}
	out := make([][]float64, len(inputs))
	for i, s := range inputs {
		v := make([]float64, f.dim)
		h := uint64(1469598103934665603)
		for j := 0; j < len(s); j++ {
			h ^= uint64(s[j])
			h *= 1099511628211
		}
		for k := range v {
			h ^= h >> 33
			h *= 0xff51afd7ed558ccd
			v[k] = float64(h%2000)/1000 - 1
		}
		out[i] = v
	}
	return out, nil
}

func (f *FixtureLLM) Complete(_ context.Context, req llm.CompleteRequest) (*llm.CompleteResponse, error) {
	// The recorded narration corresponds to the recorded chain. It is written
	// the way the real prompt asks for: conclusion first, then the chain in
	// order, naming sources, with the dispute attached to its step.
	const narration = `Meridian Holdings LLC ultimately controls Ashcroft Freight Corp, with a derivation confidence of 84%.

The chain runs through three steps. Meridian Holdings LLC reported beneficial ownership of 62.0 percent of Cobalt Capital Partners LP, per its Schedule 13D filed with the Securities Filing Registry. A holder of more than 50 percent of an issuer's voting shares controls that issuer, per the Beneficial Ownership Reporting Guide. Cobalt Capital Partners LP in turn reported beneficial ownership of 55.0 percent of Ashcroft Freight Corp — a figure Market Wire Daily disputes, reporting 71.4 percent, though the dispute does not change the control conclusion since both figures exceed the 50 percent threshold.

Corroboration on the second holding is weaker than it appears: six sources assert it, but they carry only 44% of the independent support that six unrelated sources would, because five of them trace to a single upstream origin.`

	// Detect the abstention path: the writer never calls the model in that case,
	// so reaching here with no chain would be a bug worth surfacing.
	if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "PROOF CHAIN") {
		return nil, fmt.Errorf("fixture: the writer called the model without a chain")
	}

	return &llm.CompleteResponse{
		Text: narration, Model: "fixture", FinishReason: "stop",
		PromptTokens: 640, CompletionTokens: 210,
	}, nil
}

var (
	_ kg.Reader     = (*FixtureGraph)(nil)
	_ llm.Completer = (*FixtureLLM)(nil)
	_ llm.Embedder  = (*FixtureLLM)(nil)
)
