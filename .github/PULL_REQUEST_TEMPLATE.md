<!--
Delete any section that genuinely doesn't apply (e.g. Benchmark impact for a
docs-only change). Don't leave a section in with "N/A" typed into it — either
it matters and gets answered, or it doesn't and gets removed.
-->

## Summary

What changed, and why. One or two sentences — the "why" matters more than the
"what", since the diff already shows what changed.

## Type of change

- [ ] Feature
- [ ] Fix
- [ ] Refactor (no behaviour change)
- [ ] Docs
- [ ] Chore / tooling

## Changes

- 
- 

## Testing

How you know this works. Prefer specifics over "tested it":

- [ ] `make ci` passes (gofmt + vet + `go test ./...` + Biome + tsc + next build)
- [ ] New behaviour has test coverage, or a note on why it doesn't
- [ ] Verified against a running system — say which: `make fixture` (recorded
      data) or a live FalkorDB instance, and what you actually asked it

<!--
Fixture mode exercises every layer above the graph client and the model
provider. It is fine for UI work; it proves nothing about Cypher. Anything
touching internal/kg needs a live instance or `make doctor`.
-->

## Graph impact

Required whenever this PR touches `internal/kg/**`, the schema, or an index.

- [ ] Any new query is a named template in `internal/kg/queries.go` — no Cypher
      is composed anywhere else, and no user input is concatenated into query text
- [ ] Read-path templates are `ModeRead` and contain no write clause
      (`template_test.go` covers this — confirm it's green)
- [ ] Variable-length bounds are literal, and relationship aliases are bound and
      referenced where a count matters (FalkorDB silently miscounts otherwise)
- [ ] Schema or index changes are idempotent and `argus-doctor` still passes
- [ ] Verified against a live instance, or the unverified assumption is listed
      in `docs/01-spike-results.md`

## Safety impact

Required whenever this PR touches `internal/kg/**`, `internal/extract/**`,
`internal/retract/**`, or anything else governing what the system may assert or
mutate without review.

- [ ] Confidence still only moves **down**: `EdgeKind.Calibrate` caps by kind and
      no path lets a model, prompt, or document request a higher one
- [ ] Span integrity is not weakened — no fuzzy or edit-distance matching was
      added to `FindSpan`; a quote either appears verbatim or the claim is rejected
- [ ] No new write path is reachable from a request handler. The API holds a
      `kg.Reader`; the only writer in the request path is the fork manager
- [ ] Counterfactual graphs still carry the `argus_cf_` prefix, and `DropGraph`
      still refuses the primary graph
- [ ] Retrieved document text never occupies an instruction position in a prompt
- [ ] No credential, token, or key is logged, returned in an API response, or
      committed anywhere in this diff

## Benchmark impact

Required whenever this PR touches `internal/eval/**`, `internal/corpus/**`, or
any retrieval arm.

- [ ] Published numbers in `bench/results.md` were regenerated, or the PR states
      that they are now stale
- [ ] The corpus seed is unchanged, or the change is deliberate and noted — a
      different seed produces a different world and different questions
- [ ] Every arm still runs against the same graph, corpus, model and temperature
- [ ] Grading changes apply identically to all arms, so they cannot favour ARGUS

<!--
The point of this section: it is trivially easy to improve a score by changing
the grader. If the grading logic moved, say so explicitly.
-->

## Checklist

- [ ] No secrets, API keys, or personal data anywhere in the diff
- [ ] Only public, non-personal data is loaded into the graph (hackathon rule 7)
- [ ] Docs updated if this changes setup, behaviour, the safety model, or the
      benchmark method
- [ ] Commit messages explain *why*, not just *what*
