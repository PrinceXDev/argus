package kg

// Baseline retrieval queries.
//
// These exist so the benchmark compares ARGUS against real implementations
// rather than strawmen. Every arm runs against the same graph, the same corpus
// and the same generation model, so any difference in score is attributable to
// retrieval and nothing else.
//
// Running the baselines on FalkorDB is deliberate and worth stating: it removes
// the obvious objection that ARGUS wins because its datastore is faster. It is
// the same datastore. What differs is what is asked of it.

// BaselineKeyword is arm A0: BM25-style keyword retrieval over chunks.
//
// The floor. Full-text search with no semantics and no structure.
var BaselineKeyword = register(Template{
	Name:   "baseline.keyword",
	Mode:   ModeRead,
	Doc:    "Arm A0 - keyword retrieval over chunk text via the full-text index.",
	Params: []string{"q", "limit"},
	Cypher: `
CALL db.idx.fulltext.queryNodes('Chunk', $q) YIELD node, score
MATCH (d:Document)-[:PART_OF]->(node)
RETURN node.id  AS chunkId,
       node.text AS text,
       d.title   AS documentTitle,
       score     AS score
ORDER BY score DESC
LIMIT $limit`,
})

// BaselineVector is arm A1: pure vector RAG.
//
// This is the system ARGUS is measured against, implemented properly: HNSW
// approximate nearest neighbours over chunk embeddings, top-k into the model.
// It is exactly what a vector database does, and it is a genuinely strong
// baseline on single-hop questions.
var BaselineVector = register(Template{
	Name:   "baseline.vector",
	Mode:   ModeRead,
	Doc:    "Arm A1 - pure vector RAG: ANN over chunk embeddings, top-k passages.",
	Params: []string{"k", "qvec"},
	Cypher: `
CALL db.idx.vector.queryNodes('Chunk', 'embedding', $k, vecf32($qvec))
YIELD node, score
MATCH (d:Document)-[:PART_OF]->(node)
RETURN node.id   AS chunkId,
       node.text AS text,
       d.title   AS documentTitle,
       score     AS score`,
})

// BaselineGraphNaive is arm A3: the GraphRAG most teams will build.
//
// Match entities by name, expand one hop, collect the chunks those entities
// were extracted from. This is a real technique and it beats vector search on
// entity-centric questions - which is precisely why it belongs in the
// comparison. It is not, however, a derivation: it returns a neighbourhood, and
// the model is still left to guess how the pieces connect.
var BaselineGraphNaive = register(Template{
	Name: "baseline.graph.naive",
	Mode: ModeRead,
	Doc: "Arm A3 - naive GraphRAG: entity match, one-hop expansion, source chunks. " +
		"Returns a neighbourhood rather than a derivation.",
	Params: []string{"q", "limit"},
	Cypher: `
CALL db.idx.fulltext.queryNodes('Entity', $q) YIELD node AS anchor
MATCH (anchor)-[rel:OWNS|CONTROLS|TRANSACTED_WITH|SAME_AS]-(neighbour:Entity)
WHERE ID(rel) >= 0
WITH collect(DISTINCT anchor) + collect(DISTINCT neighbour) AS ents
UNWIND ents AS e
MATCH (c:Claim)-[:ABOUT]->(e)
MATCH (c)-[:EXTRACTED_FROM]->(k:Chunk)
MATCH (d:Document)-[:PART_OF]->(k)
RETURN DISTINCT k.id  AS chunkId,
       k.text        AS text,
       d.title       AS documentTitle,
       c.conf        AS score
ORDER BY score DESC
LIMIT $limit`,
})
