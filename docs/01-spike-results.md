# Day-0 spike results — `falkordb-go`

Ran before writing product code, per §16 of the strategy doc. All four
`[VERIFY]` items from the analysis are now resolved.

## The client version trap

`go get github.com/FalkorDB/falkordb-go@latest` resolves to **v1.0.0**, which is a
single-file, redigo-based client with **no parameters, no `ROQuery`, no `Path` type,
and no vector support**. It does not match the current README on GitHub.

The real client lives at a **`/v2` module path**:

```
github.com/FalkorDB/falkordb-go/v2  →  v2.0.0 … v2.1.0
```

v2 is built on `redis/go-redis/v9`. **Always use `/v2`.** This is a trap worth an hour.

## Verification results

| # | Question | Result | Evidence |
|---|---|---|---|
| 1 | Read-only query support? | ✅ **Yes** | `func (g *Graph) ROQuery(query string, params map[string]interface{}, options *QueryOptions) (*QueryResult, error)` — `graph.go:88`. The security layer's read/write split is viable as designed. |
| 2 | `GRAPH.COPY` support? | ⚠️ **Not wrapped, but reachable** | No `CopyGraph` method. However `FalkorDB.Conn` is an exported `redis.UniversalClient` (`falkordb.go:14`), so `db.Conn.Do(ctx, "GRAPH.COPY", src, dst)` works. The library itself uses exactly this pattern for `ListGraphs`/`ConfigGet`/UDFs. **Feature B is unblocked.** |
| 3 | Vector round-tripping? | ✅ **Yes** | `TestVectorF32` asserts `RETURN vecf32([...])` decodes to `[]float32`. `TestParameterizedQuery` shows `[]interface{}` params carrying floats. So `vecf32($v)` with a `[]interface{}` of float64 is the pattern. |
| 4 | `Path` parsing from `algo.SPpaths YIELD path`? | ✅ **Yes** | Dedicated `path.go` with `GetNodes()`, `GetEdges()`, `GetNode(i)`, `GetEdge(i)`, `FirstNode()`, `LastNode()`, `NodesCount()`, `EdgeCount()`. Exactly what the proof-chain renderer needs. |

## Still open — needs a live database

| # | Question | Why it matters | Plan |
|---|---|---|---|
| 5 | Does `algo.SPpaths` `weightProp` accept **float** properties, and what type is `pathWeight`? | Docs type `pathWeight` as Integer. If float weights are rejected we must encode confidence as fixed-point `w_int = round(-ln(conf) * 1000)`. | Test as the first query against a live instance. **We are building fixed-point regardless** — it costs nothing and removes the risk. |
| 6 | Does ACL `%R~argus*` actually block a write from the query user? | The security layer's primary boundary. | `make redteam` case 1. |
| 7 | `GRAPH.COPY` latency at 50k / 500k nodes. | Determines whether Feature B forks per-retraction or uses the `conf=0` toggle fallback. | Benchmark once the graph is seeded. |

## Consequences for the build

1. **Pin `github.com/FalkorDB/falkordb-go/v2 v2.1.0`.** Never the unversioned path.
2. **`internal/kg` owns the only raw-command escape hatch.** `GRAPH.COPY` goes through
   one wrapper there so the rest of the codebase never touches `Conn` directly.
3. **Two client handles, not one.** A read-only handle used by the API and a read-write
   handle used only by the ingester — enforced by ACL at the database *and* by type at
   compile time (the API package only ever receives the RO interface).
4. **Fixed-point weights from day one.** `Claim.w_int = round(-ln(conf) * 1000)`.

## Toolchain notes (this machine)

- Go 1.26.5 ✅ · Node 22.17.0 ✅ · git ✅
- **No Docker** and **no `make`.** Consequences:
  - Local dev runs against **FalkorDB Cloud** (free tier) over TLS.
  - `docker-compose.yml` and `Makefile` still ship — judges get the one-command path.
  - `scripts/*.ps1` provide Windows parity for local development.
