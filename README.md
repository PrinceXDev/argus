# ARGUS

**Proof-carrying retrieval for investigations.**
Answers that carry their proof — and know when to stay silent.

> Built on [FalkorDB](https://www.falkordb.com) · **Graph Hacks: Building Next-Gen RAG**
> Track 01 — Investigation and Risk

---

## 30 seconds

Vector RAG returns chunks and hopes the model connects them. **ARGUS returns the chain.**

Ask _"which entity ultimately controls Ashcroft Freight Corp?"_ and ARGUS searches for the
**cheapest chain of sourced claims that entails an answer** — minimising `−ln(confidence)`,
bounded by an inferential-leap budget you control with a slider. It returns:

- the **proof chain**, every step with its source and a running probability
- the **counter-chain** — who disagrees, and whether their sources are actually independent
- the **load-bearing fact** — retract it and the verdict changes
- or **"insufficient evidence"**, with the exact place the graph runs out

|                                       | Vector RAG | Naive GraphRAG | ARGUS |
| ------------------------------------- | :--------: | :------------: | :---: |
| Returns a derivation, not chunks      |     ✗      |       ✗        |   ✓   |
| Can prove a claim is _unsupported_    |     ✗      |       ✗        |   ✓   |
| Identifies the load-bearing fact      |     ✗      |       ✗        |   ✓   |
| Detects non-independent corroboration |     ✗      |       ✗        |   ✓   |

## Why this needs a graph — and specifically FalkorDB

The load-bearing operation is **one Cypher call**:

```cypher
CALL algo.SPpaths({
  sourceNode:   src,          -- a grounded premise
  targetNode:   dst,          -- a candidate answer
  relTypes:     ['ENTAILS'],
  weightProp:   'w_int',      -- fixed-point −ln(confidence)
  costProp:     'leap',       -- inferential-leap units
  maxCost:      $leapBudget,  -- the speculation slider
  maxLen:       $maxHops,
  pathCount:    $pathCount
}) YIELD path, pathWeight, pathCost
```

Because `Σ −ln pᵢ = −ln Π pᵢ`, the **minimum-weight path is exactly the maximum-likelihood
derivation**. Finding the best explanation reduces to a shortest-path problem, and the
database returns it because "best" was encoded as "shortest" before the query ran. No
reranker, no heuristic, no second model.

`costProp`/`maxCost` is a second, independent axis: it bounds how _speculative_ a chain may
be, separately from how _likely_ it is. Two knobs, one procedure call.

**Three things FalkorDB makes possible that a vector store cannot express:**

|                                                       |                                                                                                                                                                                                                                                                                                                      |
| ----------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **The graph is the filter; the vector is the ranker** | FalkorDB documents that vector-index queries don't compose with property filters — so ARGUS inverts the usual order. It traverses to a structurally-justified candidate set first, then ranks _inside_ it with `vec.cosineDistance` in the same query. The filter is a path predicate. Reachability is not a column. |
| **Negative knowledge is a first-class result**        | "No path exists inside this budget" is checkable. A top-k search always returns something and structurally cannot decline.                                                                                                                                                                                           |
| **Worlds are forkable**                               | `GRAPH.COPY` clones the graph while the source stays readable, so ARGUS ablates one claim, re-derives, and diffs — with **zero model calls**.                                                                                                                                                                        |

Delete FalkorDB and ARGUS does not degrade. It stops having a product.

## Work the graph does

The brief lists five jobs. ARGUS does all five, and here is the code for each:

| Brief                    | ARGUS feature             | Implementation                                                                                |
| ------------------------ | ------------------------- | --------------------------------------------------------------------------------------------- |
| Finding a path           | Proof chains              | [`prove.chain`](internal/kg/queries.go) → [`internal/prove`](internal/prove/prove.go)         |
| Tracing an impact        | Counterfactual retraction | [`retract.claim`](internal/kg/queries.go) → [`internal/retract`](internal/retract/retract.go) |
| Grouping related records | Fraud-ring detection      | [`rings.communities`](internal/kg/queries.go) — `algo.labelPropagation`                       |
| Ranking a network        | Source trust              | [`court.sourceTrust`](internal/kg/queries.go) — `algo.pageRank`                               |
| Searching connected data | Scoped hybrid retrieval   | [`candidate.claims.scoped`](internal/kg/queries.go)                                           |

Plus `algo.SSpaths` for the evidence frontier and `algo.maxFlow` for corroboration
independence. **31 named queries**, all served live at `GET /api/catalogue` so the
submitted Cypher cannot drift from the running Cypher.

## The three features

### A — Proof chains _(flagship)_

Every answer is a derivation from a grounded premise to a conclusion, each step weighted by
`−ln(confidence)` and priced in leap budget. Drag the budget down and the system stops
mid-derivation and says **insufficient evidence** — showing the frontier claims where the
corpus runs out, so a refusal is a research lead rather than a shrug.

### B — Counterfactual retraction

`GRAPH.COPY` forks the graph, one claim is ablated, the derivation re-runs, and the
confidence delta is measured. Ranks every fact by how much of the conclusion collapses
without it. **No language model is involved** — the question vector is computed once and
every re-derivation is pure graph computation.

### C — The court

Contradictions are edges, so surfacing a dispute is a lookup. Source trust is `algo.pageRank`
over the citation network, not a constant someone typed in. Corroboration is `algo.maxFlow`
from origin publishers — which is how ARGUS can say _"six sources assert this, but they carry
only 44% of the independent support that six unrelated sources would; five trace to one
origin."_ That is a max-flow bottleneck, not a count.

## Run it

**No database, no API key** — recorded data through the real request path:

```bash
make fixture          # API on :8080
make web              # UI on :3000
```

**The real thing:**

```bash
cp .env.example .env  # add FALKORDB_URL and LLM_API_KEY
make demo             # docker compose + schema + seed the synthetic corpus
make api              # and `make web` in another shell
```

On Windows without `make`: `.\scripts\dev.ps1 doctor | test | api | web`

## Evidence

```bash
make bench            # 5 arms, same graph, same corpus, same model → bench/results.md
```

Five arms compete on identical terms — **all against the same FalkorDB instance**, which
removes the objection that ARGUS wins because its datastore is faster:

| Arm              | Method                                                                      |
| ---------------- | --------------------------------------------------------------------------- |
| `A0-keyword`     | BM25-style full-text over chunks                                            |
| `A1-vector`      | Pure vector RAG, HNSW over chunk embeddings                                 |
| `A2-hybrid`      | Reciprocal rank fusion of both                                              |
| `A3-graph-naive` | Entity match + one-hop expansion (the GraphRAG most teams build)            |
| `A5-argus`       | Traversal-scoped candidates, cost-bounded derivation, calibrated abstention |

Scored over **94 questions in 7 classes** generated from a ground-truth world, including two
that no baseline can score on by construction:

- **L6 unanswerable** — the evidence was deliberately withheld, so declining is the only
  correct answer
- **L7 poisoned** — a false claim from an uncorroborated source; repeating it fails even if
  the rest of the answer is perfect

Metrics: accuracy (answerable only), **abstention F1**, **poison attack success rate**,
**attributable precision**, over-answering count, and graph-vs-model latency split.

> **No numbers are published here yet.** The harness ships in the repository and every figure
> in `bench/results.md` is produced by running it. Filling in a table before the run would
> defeat the purpose. `bench/results.md` includes its own Limitations section.

## Architecture

```
Next.js 16 · React 19 · Tailwind 4          SSE: chain streams before prose
        │
        ▼
Go 1.26 API ── prove ── retract ── court ── answer
        │        │         │         │        └── the only user-facing LLM call
        │        │         │         └── pageRank · maxFlow · contradictions
        │        │         └── GRAPH.COPY forks · zero LLM calls
        │        └── SPpaths / SSpaths over −ln(confidence)
        ▼
   internal/kg ── 31 parameterised templates, validated at startup
        │
        ▼
     FalkorDB
```

One database. No Postgres, no Pinecone, no second store.

## Security

ARGUS deliberately does **not** implement free-form text-to-Cypher. Every query is a named,
parameterised template validated at process start; an LLM may _select_ a template and fill
typed parameters, but never authors query text. An LLM that emits executable Cypher is a
remote-code-execution surface, and FalkorDB's own retrieval benchmark measures text-to-Cypher
as worth **+0.4% accuracy for +1.5s latency** — which does not buy the risk.

Four layers, weakest to strongest: prompt instruction → instruction/data separation →
schema-constrained output → **span integrity and confidence capping**. The design rule is
that no security property may rest on the first two alone.

`make redteam` runs the adversarial suite. The full threat model — including a section on
what span integrity **cannot** do — is in [`docs/02-threat-model.md`](docs/02-threat-model.md).

## Status

| Component                                                        | State                  |
| ---------------------------------------------------------------- | ---------------------- |
| `internal/kg` — client, read/write split, 31 validated templates | ✅                     |
| `internal/extract` — calibrated extraction, span integrity       | ✅                     |
| `internal/ingest` — chunking, deterministic IDs, graph writer    | ✅                     |
| `internal/corpus` — ground-truth world + 94-question benchmark   | ✅                     |
| **Feature A** — proof chains + abstention                        | ✅                     |
| **Feature B** — counterfactual retraction                        | ✅                     |
| **Feature C** — disputes, trust, corroboration flow              | ✅                     |
| `internal/eval` — 5-arm harness, metrics, report                 | ✅                     |
| API (SSE) · Web UI · fixture mode                                | ✅                     |
| **Verified against a live FalkorDB instance**                    | ⬜ pending credentials |
| Benchmark executed, numbers published                            | ⬜ pending credentials |

**177 tests.** `go build ./...`, `go vet ./...` and `npm run build` all clean.

Three query-level assumptions remain unverified without a live instance and are listed in
[`docs/01-spike-results.md`](docs/01-spike-results.md#still-open). `argus-doctor` tests all of
them on first connection.

## Documentation

|                                                                |                                                                          |
| -------------------------------------------------------------- | ------------------------------------------------------------------------ |
| [`docs/00-strategy-analysis.md`](docs/00-strategy-analysis.md) | Competition analysis, thesis derivation, feature specs, benchmark design |
| [`docs/01-spike-results.md`](docs/01-spike-results.md)         | Day-0 verification of the Go client against the design                   |
| [`docs/02-threat-model.md`](docs/02-threat-model.md)           | Attack register, layered defence, and what is _not_ mitigated            |

## Data

- **SEC EDGAR** — real filings, US public domain. Fetched with a rate limiter and a
  contact User-Agent, as the SEC requires.
- **Synthetic corpus** — deterministic from a seed, with a ground-truth world. This is the
  only way to score abstention and poisoning honestly: the evidence must be _provably_
  absent, and the false claim _known_ to be false.

## AI assistance disclosure

Per the hackathon rules. This project was built with substantial AI assistance (Claude) for
implementation, and the design decisions, verification and review are the author's. Every
architectural claim in this README traces to a primary source cited in
[`docs/00-strategy-analysis.md`](docs/00-strategy-analysis.md); the FalkorDB capabilities
relied on were verified against the official documentation and, where the Go client was
concerned, against the library source before being designed around
([`docs/01-spike-results.md`](docs/01-spike-results.md)). Assumptions that could not be
verified are labelled as such rather than asserted.

## Licence

MIT. See [`LICENSE`](LICENSE).
