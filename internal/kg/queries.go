package kg

// The ARGUS query catalogue.
//
// Every query the product depends on is declared here as a compile-time
// constant and validated at process start. Nothing else in ARGUS composes
// Cypher. The hackathon requires submitting "the Cypher queries or graph
// algorithms the product depends on"; this file is that submission, and
// `argus-doctor -catalogue` renders it into the README.
//
// Two conventions apply throughout:
//
//   - Relationship aliases are always bound and referenced. FalkorDB only
//     existence-checks a relationship that appears nowhere else in the query,
//     which silently corrupts counts. Where a count matters we reference the
//     alias explicitly.
//   - Variable-length bounds are literal. openCypher does not allow a
//     parameter in a `*1..n` range, so traversal depth is fixed per template
//     rather than passed in. TraversalDepth documents the chosen value.
const TraversalDepth = 2

// ─────────────────────────────────────────────────────────────────────────────
// Anchoring: question text -> entities in the graph
// ─────────────────────────────────────────────────────────────────────────────

// AnchorEntityFulltext finds entities by keyword. Stemming and partial matches
// come from the RediSearch-backed full-text index, which handles the cases an
// embedding is poor at: exact company names, tickers, and CIK numbers.
var AnchorEntityFulltext = register(Template{
	Name:   "anchor.entity.fulltext",
	Mode:   ModeRead,
	Doc:    "Keyword-anchor a question to entities using the full-text index.",
	Params: []string{"q", "limit"},
	Cypher: `
CALL db.idx.fulltext.queryNodes('Entity', $q) YIELD node, score
RETURN node.id            AS id,
       node.canonical_name AS name,
       node.type          AS type,
       score              AS score
ORDER BY score DESC
LIMIT $limit`,
})

// AnchorEntityVector finds entities by meaning, for questions that describe an
// entity without naming it ("the Delaware holding company that owns...").
//
// This is the one place ARGUS uses the ANN index: a global entry point, where
// there is no traversal to scope by yet. Everything downstream ranks with
// vec.cosineDistance inside a graph-scoped candidate set instead, because
// FalkorDB documents that vector index queries do not compose with filters.
var AnchorEntityVector = register(Template{
	Name:   "anchor.entity.vector",
	Mode:   ModeRead,
	Doc:    "Semantic-anchor a question to entities using the HNSW vector index.",
	Params: []string{"k", "qvec"},
	Cypher: `
CALL db.idx.vector.queryNodes('Entity', 'embedding', $k, vecf32($qvec))
YIELD node, score
RETURN node.id             AS id,
       node.canonical_name AS name,
       node.type           AS type,
       score               AS score`,
})

// ─────────────────────────────────────────────────────────────────────────────
// Candidate generation: the signature query
// ─────────────────────────────────────────────────────────────────────────────

// CandidateClaimsScoped is the query that states the thesis.
//
// A vector database ranks the whole corpus by embedding distance and then
// post-filters on metadata columns. ARGUS inverts that: it first traverses to
// the set of claims that are *structurally* relevant - claims about entities
// reachable from the question's anchors through ownership, control, transaction
// or identity edges - and only then ranks that set semantically, with
// vec.cosineDistance evaluated inside the same query.
//
// The filter is a path predicate. There is no way to express it in a vector
// store, because reachability is not a column. The candidate set is also small
// enough that exact cosine beats approximate search on both cost and recall.
//
// The temporal predicate makes the whole thing as-of a date, so "who controlled
// X in March 2019" cannot be answered with a 2024 restatement.
var CandidateClaimsScoped = register(Template{
	Name: "candidate.claims.scoped",
	Mode: ModeRead,
	Doc: "Traverse from anchor entities to structurally-relevant claims, then rank " +
		"them semantically with vec.cosineDistance. The graph is the filter; the " +
		"vector is the ranker.",
	Params: []string{"anchorIDs", "qvec", "asOf", "maxDist", "limit"},
	Cypher: `
UNWIND $anchorIDs AS aid
MATCH (a:Entity {id: aid})
MATCH (a)-[rel:OWNS|CONTROLS|TRANSACTED_WITH|SAME_AS*0..2]-(e:Entity)
WITH DISTINCT e, size(rel) AS hops
MATCH (c:Claim)-[:ABOUT]->(e)
WHERE c.valid_from <= $asOf
  AND (c.valid_to IS NULL OR c.valid_to >= $asOf)
WITH DISTINCT c, min(hops) AS hops
WITH c, hops, vec.cosineDistance(c.embedding, vecf32($qvec)) AS dist
WHERE dist <= $maxDist
RETURN c.id   AS id,
       c.text AS text,
       c.conf AS conf,
       hops   AS hops,
       dist   AS dist
ORDER BY dist ASC
LIMIT $limit`,
})

// ─────────────────────────────────────────────────────────────────────────────
// Feature A: proof chains
// ─────────────────────────────────────────────────────────────────────────────

// ProveChain is the flagship query.
//
// Edge weights are fixed-point -ln(confidence). Because sum(-ln p_i) equals
// -ln(prod p_i), the minimum-weight path is exactly the maximum-likelihood
// derivation - so "find the best explanation" reduces to a shortest-path
// problem, which FalkorDB solves natively in one call.
//
// costProp/maxCost is a second, independent axis: `leap` counts inferential
// leaps, so a user can demand a less speculative answer without changing what
// "most likely" means. Two knobs, one procedure call.
//
// Returning zero rows is a meaningful result, not a failure: it means no
// derivation exists inside the budget, which is what licenses abstention.
var ProveChain = register(Template{
	Name: "prove.chain",
	Mode: ModeRead,
	Doc: "Find the cheapest chains of sourced claims entailing a candidate answer, " +
		"minimising -ln(confidence) subject to an inferential-leap budget.",
	Params: []string{"srcID", "dstID", "leapBudget", "maxHops", "pathCount"},
	Cypher: `
MATCH (src:Claim {id: $srcID})
MATCH (dst:Claim {id: $dstID})
CALL algo.SPpaths({
  sourceNode:   src,
  targetNode:   dst,
  relTypes:     ['ENTAILS'],
  weightProp:   'w_int',
  costProp:     'leap',
  maxCost:      $leapBudget,
  maxLen:       $maxHops,
  relDirection: 'outgoing',
  pathCount:    $pathCount
})
YIELD path, pathWeight, pathCost
RETURN pathWeight                          AS weight,
       pathCost                            AS cost,
       length(path)                        AS hops,
       [n IN nodes(path) | n.id]           AS claimIDs,
       [n IN nodes(path) | n.text]         AS claimTexts,
       [r IN relationships(path) | r.w_int] AS stepWeights
ORDER BY pathWeight ASC`,
})

// ProveRoots finds the grounded premises a derivation can start from.
//
// ENTAILS points premise -> conclusion, so a derivation is a path from a claim
// with no incoming inference edge - one asserted directly by a document rather
// than inferred from other claims - to a candidate answer. Those roots are the
// axioms of the corpus, and starting anywhere else would let a chain rest on
// something the graph itself never grounded.
var ProveRoots = register(Template{
	Name: "prove.roots",
	Mode: ModeRead,
	Doc: "Find grounded root claims (directly asserted, not inferred) about the " +
		"anchor entities - the legal starting points for a derivation.",
	Params: []string{"anchorIDs", "asOf", "minConf", "limit"},
	Cypher: `
UNWIND $anchorIDs AS aid
MATCH (a:Entity {id: aid})
MATCH (c:Claim)-[:ABOUT]->(a)
WHERE NOT (c)<-[:ENTAILS]-()
  AND c.conf >= $minConf
  AND c.valid_from <= $asOf
  AND (c.valid_to IS NULL OR c.valid_to >= $asOf)
RETURN DISTINCT c.id AS id,
       c.text        AS text,
       c.conf        AS conf
ORDER BY c.conf DESC
LIMIT $limit`,
})

// ProveReach is phase one of the two-phase proof search.
//
// Running SPpaths for every (root, candidate) pair would be O(roots x
// candidates) round trips. Instead SSpaths explores everything derivable from a
// single root inside the budget in one call, and the engine intersects the
// reachable tips with its candidate set in Go. That makes discovery O(roots)
// queries; only the winning pair is then reconstructed in full by ProveChain.
//
// pathCount is 0 (all shortest paths), which is safe because maxCost and maxLen
// bound the exploration - the leap budget is doing double duty as a
// correctness knob and a cost ceiling.
var ProveReach = register(Template{
	Name: "prove.reach",
	Mode: ModeRead,
	Doc: "Phase one of proof search: everything derivable from one root inside the " +
		"leap budget, as (tip, weight, cost) triples.",
	Params: []string{"srcID", "leapBudget", "maxHops"},
	Cypher: `
MATCH (src:Claim {id: $srcID})
CALL algo.SSpaths({
  sourceNode:   src,
  relTypes:     ['ENTAILS'],
  weightProp:   'w_int',
  costProp:     'leap',
  maxCost:      $leapBudget,
  maxLen:       $maxHops,
  relDirection: 'outgoing',
  pathCount:    0
})
YIELD path, pathWeight, pathCost
WITH last(nodes(path)) AS tip, pathWeight AS weight, pathCost AS cost, length(path) AS hops
RETURN tip.id       AS id,
       min(weight)  AS weight,
       min(cost)    AS cost,
       min(hops)    AS hops`,
})

// ProveFrontier explains an abstention.
//
// When ProveChain returns nothing, the useful answer is not "I don't know" but
// "here is how far the evidence reaches and where it stops." SSpaths enumerates
// everything derivable from the anchor inside the budget; the terminal node of
// each path is a frontier claim - the edge of what the corpus supports.
//
// Showing the frontier turns a refusal into a research lead, which is what an
// analyst actually wants.
var ProveFrontier = register(Template{
	Name: "prove.frontier",
	Mode: ModeRead,
	Doc: "Enumerate the reachable frontier from an anchor claim inside the leap " +
		"budget, to show exactly where the evidence runs out.",
	Params: []string{"srcID", "leapBudget", "maxHops", "qvec", "limit"},
	Cypher: `
MATCH (src:Claim {id: $srcID})
CALL algo.SSpaths({
  sourceNode:   src,
  relTypes:     ['ENTAILS'],
  weightProp:   'w_int',
  costProp:     'leap',
  maxCost:      $leapBudget,
  maxLen:       $maxHops,
  relDirection: 'outgoing',
  pathCount:    0
})
YIELD path, pathWeight, pathCost
WITH last(nodes(path)) AS tip, pathWeight AS weight, pathCost AS cost, length(path) AS hops
WITH tip, min(weight) AS weight, min(cost) AS cost, min(hops) AS hops
RETURN tip.id   AS id,
       tip.text AS text,
       tip.conf AS conf,
       weight   AS weight,
       cost     AS cost,
       hops     AS hops,
       vec.cosineDistance(tip.embedding, vecf32($qvec)) AS dist
ORDER BY dist ASC
LIMIT $limit`,
})

// ChainEvidence hydrates a proof chain with its provenance.
//
// The MANDATORY constraint on ASSERTED_BY.span guarantees every row here has a
// character offset: a claim that cannot point at the text it came from cannot
// exist in the graph at all. That is what makes the chain auditable rather than
// merely plausible.
var ChainEvidence = register(Template{
	Name:   "chain.evidence",
	Mode:   ModeRead,
	Doc:    "Resolve each claim on a proof chain to its source document, span and publisher.",
	Params: []string{"claimIDs"},
	Cypher: `
UNWIND $claimIDs AS cid
MATCH (c:Claim {id: cid})-[a:ASSERTED_BY]->(d:Document)-[:PUBLISHED_BY]->(s:Source)
RETURN c.id         AS claimID,
       c.text       AS claimText,
       c.conf       AS conf,
       c.valid_from AS validFrom,
       c.valid_to   AS validTo,
       a.span       AS span,
       d.id         AS documentID,
       d.title      AS documentTitle,
       d.url        AS documentURL,
       s.id         AS sourceID,
       s.name       AS sourceName,
       s.kind       AS sourceKind,
       s.trust      AS sourceTrust`,
})

// ─────────────────────────────────────────────────────────────────────────────
// Feature C: the court
// ─────────────────────────────────────────────────────────────────────────────

// CourtContradictions finds claims that dispute anything on a proof chain.
//
// A conventional RAG system silently picks whichever contradictory passage
// ranked higher and never tells the user a dispute existed. Here the dispute is
// an edge, so surfacing it is a lookup rather than an inference.
var CourtContradictions = register(Template{
	Name:   "court.contradictions",
	Mode:   ModeRead,
	Doc:    "Find claims contradicting any claim on a proof chain, with their sources.",
	Params: []string{"claimIDs"},
	Cypher: `
UNWIND $claimIDs AS cid
MATCH (c:Claim {id: cid})-[x:CONTRADICTS]-(other:Claim)
MATCH (other)-[:ASSERTED_BY]->(d:Document)-[:PUBLISHED_BY]->(s:Source)
RETURN c.id            AS claimID,
       other.id        AS counterID,
       other.text      AS counterText,
       other.conf      AS counterConf,
       x.detected_by   AS detectedBy,
       s.id            AS sourceID,
       s.name          AS sourceName,
       s.trust         AS sourceTrust
ORDER BY other.conf DESC`,
})

// CourtSourceTrust ranks sources by their position in the citation network.
//
// Source credibility is treated as a network property rather than a constant an
// engineer typed in. A source that nothing corroborates cannot bootstrap its own
// authority, which is what starves a poisoned or sock-puppet source of weight.
var CourtSourceTrust = register(Template{
	Name:   "court.sourceTrust",
	Mode:   ModeRead,
	Doc:    "Rank sources by PageRank over the citation/corroboration network.",
	Params: []string{"limit"},
	Cypher: `
CALL algo.pageRank('Source', 'CITES') YIELD node, score
RETURN node.id   AS id,
       node.name AS name,
       node.kind AS kind,
       score     AS score
ORDER BY score DESC
LIMIT $limit`,
})

// RingCommunities groups entities into densely-connected clusters.
//
// This is the hackathon brief's "grouping related records", and in the
// investigation domain it is fraud-ring detection: label propagation over
// ownership and transaction edges surfaces clusters that no single record
// describes.
var RingCommunities = register(Template{
	Name:   "rings.communities",
	Mode:   ModeRead,
	Doc:    "Detect entity communities (fraud rings) via label propagation.",
	Params: []string{"minSize"},
	Cypher: `
CALL algo.labelPropagation({
  nodeLabels:        ['Entity'],
  relationshipTypes: ['OWNS', 'CONTROLS', 'TRANSACTED_WITH'],
  maxIterations:     20
})
YIELD node, communityId
WITH communityId, collect(node.canonical_name) AS members, count(node) AS n
WHERE n >= $minSize
RETURN communityId AS id, n AS size, members AS members
ORDER BY n DESC`,
})

// GraphStats feeds the latency/scale HUD.
var GraphStats = register(Template{
	Name:   "stats.meta",
	Mode:   ModeRead,
	Doc:    "Graph-wide counts for the UI header.",
	Params: nil,
	Cypher: `
CALL db.meta.stats()
YIELD nodeCount, relCount, labelCount, relTypeCount
RETURN nodeCount, relCount, labelCount, relTypeCount`,
})

// ─────────────────────────────────────────────────────────────────────────────
// Feature B: retraction (executed against a fork, never the primary graph)
// ─────────────────────────────────────────────────────────────────────────────

// RetractClaim ablates a claim inside a counterfactual fork.
//
// Setting confidence to zero rather than deleting the node keeps the graph shape
// intact, so the re-derivation measures the effect of *disbelieving* the claim
// rather than the effect of a structural hole. Detaching ENTAILS edges is what
// actually removes it from the derivation.
var RetractClaim = register(Template{
	Name:   "retract.claim",
	Mode:   ModeWrite,
	Doc:    "Ablate one claim inside a fork: zero its confidence and cut its inference edges.",
	Params: []string{"claimID"},
	Cypher: `
MATCH (c:Claim {id: $claimID})
SET c.conf = 0.0, c.retracted = true
WITH c
MATCH (c)-[r:ENTAILS]-()
DELETE r
RETURN count(r) AS edgesRemoved`,
})

// ─────────────────────────────────────────────────────────────────────────────
// Ingestion
// ─────────────────────────────────────────────────────────────────────────────

var SourceUpsert = register(Template{
	Name:   "source.upsert",
	Mode:   ModeWrite,
	Doc:    "Create or update a publisher of documents.",
	Params: []string{"id", "name", "kind", "trust"},
	Cypher: `
MERGE (s:Source {id: $id})
SET s.name = $name, s.kind = $kind, s.trust = $trust
RETURN s.id AS id`,
})

var DocumentUpsert = register(Template{
	Name:   "document.upsert",
	Mode:   ModeWrite,
	Doc:    "Create or update a source document and link it to its publisher.",
	Params: []string{"id", "sourceID", "title", "url", "publishedAt", "sha256"},
	Cypher: `
MATCH (s:Source {id: $sourceID})
MERGE (d:Document {id: $id})
SET d.title = $title, d.url = $url, d.published_at = $publishedAt, d.sha256 = $sha256
MERGE (d)-[:PUBLISHED_BY]->(s)
RETURN d.id AS id`,
})

var ChunkUpsert = register(Template{
	Name:   "chunk.upsert",
	Mode:   ModeWrite,
	Doc:    "Create or update a text chunk with its embedding, linked to its document.",
	Params: []string{"id", "documentID", "text", "index", "startChar", "endChar", "vec"},
	Cypher: `
MATCH (d:Document {id: $documentID})
MERGE (k:Chunk {id: $id})
SET k.text       = $text,
    k.index      = $index,
    k.start_char = $startChar,
    k.end_char   = $endChar,
    k.embedding  = vecf32($vec)
MERGE (d)-[:PART_OF]->(k)
RETURN k.id AS id`,
})

var EntityUpsert = register(Template{
	Name:   "entity.upsert",
	Mode:   ModeWrite,
	Doc:    "Create or update a resolved entity with its name embedding.",
	Params: []string{"id", "name", "type", "vec"},
	Cypher: `
MERGE (e:Entity {id: $id})
SET e.canonical_name = $name,
    e.type           = $type,
    e.embedding      = vecf32($vec)
RETURN e.id AS id`,
})

// ClaimUpsert writes an assertion together with its provenance in one atomic
// query. FalkorDB makes every query atomic, so a claim can never be persisted
// without the ASSERTED_BY edge that the MANDATORY constraint requires - the
// write either lands complete or not at all.
var ClaimUpsert = register(Template{
	Name: "claim.upsert",
	Mode: ModeWrite,
	Doc:  "Write a claim with its embedding, provenance span and validity window, atomically.",
	Params: []string{"id", "text", "conf", "validFrom", "validTo", "vec",
		"documentID", "chunkID", "span"},
	Cypher: `
MATCH (d:Document {id: $documentID})
MATCH (k:Chunk {id: $chunkID})
MERGE (c:Claim {id: $id})
SET c.text       = $text,
    c.conf       = $conf,
    c.valid_from = $validFrom,
    c.valid_to   = $validTo,
    c.embedding  = vecf32($vec)
MERGE (c)-[a:ASSERTED_BY]->(d)
SET a.span = $span
MERGE (c)-[:EXTRACTED_FROM]->(k)
RETURN c.id AS id`,
})

var ClaimAbout = register(Template{
	Name:   "claim.about",
	Mode:   ModeWrite,
	Doc:    "Link a claim to an entity it concerns.",
	Params: []string{"claimID", "entityID"},
	Cypher: `
MATCH (c:Claim {id: $claimID})
MATCH (e:Entity {id: $entityID})
MERGE (c)-[:ABOUT]->(e)
RETURN c.id AS id`,
})

// EntailsUpsert writes an inference edge. w_int and leap are supplied by the
// caller rather than computed here, because WeightFromConfidence is the single
// definition of how confidence becomes distance and it lives in Go where it can
// be unit-tested.
var EntailsUpsert = register(Template{
	Name:   "entails.upsert",
	Mode:   ModeWrite,
	Doc:    "Write an inference edge carrying its fixed-point weight and leap cost.",
	Params: []string{"fromID", "toID", "wInt", "leap", "conf", "rationale"},
	Cypher: `
MATCH (a:Claim {id: $fromID})
MATCH (b:Claim {id: $toID})
MERGE (a)-[r:ENTAILS]->(b)
SET r.w_int     = $wInt,
    r.leap      = $leap,
    r.conf      = $conf,
    r.rationale = $rationale
RETURN r.w_int AS wInt`,
})

var ContradictsUpsert = register(Template{
	Name:   "contradicts.upsert",
	Mode:   ModeWrite,
	Doc:    "Record that two claims are mutually inconsistent.",
	Params: []string{"aID", "bID", "detectedBy", "conf"},
	Cypher: `
MATCH (a:Claim {id: $aID})
MATCH (b:Claim {id: $bID})
MERGE (a)-[r:CONTRADICTS]->(b)
SET r.detected_by = $detectedBy, r.conf = $conf
RETURN r.conf AS conf`,
})

// SupersedesUpsert records a temporal restatement: same fact, later filing.
// This is what stops a 2024 amendment from answering a question about 2019.
var SupersedesUpsert = register(Template{
	Name:   "supersedes.upsert",
	Mode:   ModeWrite,
	Doc:    "Record that a later claim restates and replaces an earlier one.",
	Params: []string{"newID", "oldID", "at"},
	Cypher: `
MATCH (n:Claim {id: $newID})
MATCH (o:Claim {id: $oldID})
MERGE (n)-[r:SUPERSEDES]->(o)
SET r.at = $at, o.valid_to = $at
RETURN r.at AS at`,
})

// SameAsUpsert records an identity-resolution decision, keeping the score and
// the method that produced it so a merge can be reviewed rather than trusted.
var SameAsUpsert = register(Template{
	Name:   "sameas.upsert",
	Mode:   ModeWrite,
	Doc:    "Record an identity-resolution link between two entity records.",
	Params: []string{"aID", "bID", "score", "method"},
	Cypher: `
MATCH (a:Entity {id: $aID})
MATCH (b:Entity {id: $bID})
MERGE (a)-[r:SAME_AS]->(b)
SET r.score = $score, r.method = $method
RETURN r.score AS score`,
})

// EntityRelate writes a domain relationship between two entities.
// The relationship type is fixed per template rather than parameterised,
// because a parameterised relationship type would require string-building the
// query - exactly the injection surface this package exists to eliminate.
var (
	EntityOwns = register(Template{
		Name:   "entity.owns",
		Mode:   ModeWrite,
		Doc:    "Record an ownership relationship between two entities.",
		Params: []string{"fromID", "toID", "pct", "at", "documentID"},
		Cypher: `
MATCH (a:Entity {id: $fromID})
MATCH (b:Entity {id: $toID})
MERGE (a)-[r:OWNS]->(b)
SET r.pct = $pct, r.at = $at, r.document_id = $documentID
RETURN r.pct AS pct`,
	})

	EntityControls = register(Template{
		Name:   "entity.controls",
		Mode:   ModeWrite,
		Doc:    "Record a control relationship between two entities.",
		Params: []string{"fromID", "toID", "role", "at", "documentID"},
		Cypher: `
MATCH (a:Entity {id: $fromID})
MATCH (b:Entity {id: $toID})
MERGE (a)-[r:CONTROLS]->(b)
SET r.role = $role, r.at = $at, r.document_id = $documentID
RETURN r.role AS role`,
	})

	EntityTransacted = register(Template{
		Name:   "entity.transacted",
		Mode:   ModeWrite,
		Doc:    "Record a transaction between two entities.",
		Params: []string{"fromID", "toID", "amount", "at", "documentID"},
		Cypher: `
MATCH (a:Entity {id: $fromID})
MATCH (b:Entity {id: $toID})
MERGE (a)-[r:TRANSACTED_WITH]->(b)
SET r.amount = $amount, r.at = $at, r.document_id = $documentID
RETURN r.amount AS amount`,
	})
)

var SourceCites = register(Template{
	Name:   "source.cites",
	Mode:   ModeWrite,
	Doc:    "Record that one source cites another, feeding the trust PageRank.",
	Params: []string{"fromID", "toID"},
	Cypher: `
MATCH (a:Source {id: $fromID})
MATCH (b:Source {id: $toID})
MERGE (a)-[r:CITES]->(b)
RETURN count(r) AS n`,
})

// ─────────────────────────────────────────────────────────────────────────────
// Feature C: corroboration flow
//
// Measuring how much *independent* support a claim has is a flow problem, not a
// counting problem. Ten outlets that all republish one press release are one
// source, and counting them gives ten. Max-flow gives one, because every path
// is bottlenecked by the shared origin's capacity.
//
// The flow network runs Origin -> Outlet -> Claim:
//
//	(:Source)-[:CITED_BY {capacity}]->(:Source)-[:CORROBORATES {capacity}]->(:Claim)
//
// Both edge types are materialised after ingest rather than written inline,
// because capacity depends on PageRank trust, which cannot be known until the
// whole citation network exists.
// ─────────────────────────────────────────────────────────────────────────────

// MaterialiseCorroboration builds the Source -> Claim edges of the flow network.
//
// Capacity is integer because algo.maxFlow sums capacities into an integer
// maxFlow; trust is scaled by 100 so a 0.95-trust filing carries 95 units and a
// 0.1-trust blog carries 10.
var MaterialiseCorroboration = register(Template{
	Name: "court.materialise.corroborates",
	Mode: ModeWrite,
	Doc: "Materialise Source->Claim corroboration edges with trust-scaled integer " +
		"capacities, for max-flow analysis.",
	Params: nil,
	Cypher: `
MATCH (c:Claim)-[:ASSERTED_BY]->(:Document)-[:PUBLISHED_BY]->(s:Source)
MERGE (s)-[r:CORROBORATES]->(c)
SET r.capacity = toInteger(coalesce(s.trust, 0.5) * 100)
RETURN count(r) AS edges`,
})

// MaterialiseCitedBy reverses CITES so flow runs from origin to outlet.
//
// A citing outlet cannot contribute more independent support than the origin it
// draws on, and reversing the edge is what enforces that: all of an outlet's
// flow must pass through the source it cites.
var MaterialiseCitedBy = register(Template{
	Name:   "court.materialise.citedby",
	Mode:   ModeWrite,
	Doc:    "Materialise reversed citation edges so corroboration flow is bottlenecked by shared origins.",
	Params: nil,
	Cypher: `
MATCH (outlet:Source)-[:CITES]->(origin:Source)
MERGE (origin)-[r:CITED_BY]->(outlet)
SET r.capacity = toInteger(coalesce(origin.trust, 0.5) * 100)
RETURN count(r) AS edges`,
})

// CourtCorroboration measures independent support for one claim.
//
// Sources are the origins - publishers that cite nobody, so they are the actual
// root of whatever evidence exists. maxFlow from them to the claim is bounded
// by the narrowest cut, which is precisely the "six reports, one origin" case.
var CourtCorroboration = register(Template{
	Name: "court.corroboration",
	Mode: ModeRead,
	Doc: "Max-flow from origin sources to one claim, measuring independent " +
		"corroboration and exposing the bottleneck.",
	Params: []string{"claimID"},
	Cypher: `
MATCH (c:Claim {id: $claimID})
MATCH (origin:Source)
WHERE NOT (origin)-[:CITES]->(:Source)
  AND (origin)-[:CORROBORATES|CITED_BY]->()
WITH c, collect(origin) AS origins
WHERE size(origins) > 0
CALL algo.maxFlow({
  sourceNodes:       origins,
  targetNodes:       [c],
  relationshipTypes: ['CITED_BY', 'CORROBORATES'],
  capacityProperty:  'capacity',
  nodeLabels:        ['Source', 'Claim']
})
YIELD edges, edgeFlows, maxFlow
RETURN maxFlow                                     AS flow,
       size(origins)                               AS originCount,
       [e IN edges | startNode(e).name]            AS fromNames,
       [e IN edges | type(e)]                      AS edgeTypes,
       edgeFlows                                   AS flows`,
})

// CourtDirectSupport counts the sources that assert a claim, without regard to
// independence. Reported alongside the flow so the gap between the two - six
// sources, one unit of flow - is visible rather than implied.
var CourtDirectSupport = register(Template{
	Name:   "court.directSupport",
	Mode:   ModeRead,
	Doc:    "List every source asserting a claim, with its trust, ignoring independence.",
	Params: []string{"claimID"},
	Cypher: `
MATCH (c:Claim {id: $claimID})-[:ASSERTED_BY]->(:Document)-[:PUBLISHED_BY]->(s:Source)
RETURN DISTINCT s.id AS id, s.name AS name, s.kind AS kind, s.trust AS trust
ORDER BY s.trust DESC`,
})
