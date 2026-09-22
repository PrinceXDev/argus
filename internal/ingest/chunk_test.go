package ingest

import (
	"strings"
	"testing"
)

const filing = `Zeta Holdings LLC reported a 60.5% beneficial ownership stake in Acme Corp. ` +
	`The stake was acquired on March 14, 2019 through a series of open-market purchases. ` +
	`Dana Reyes, Chief Financial Officer of Zeta Holdings LLC, signed the filing. ` +
	`Zeta Holdings LLC is organised under the laws of the U.S. state of Delaware. ` +
	`No. 5 Harbour Road serves as the registered address.`

// Offsets are load-bearing: span integrity resolves a quote inside a chunk, and
// the UI highlights it in the source. A chunker that loses offsets breaks both.
func TestChunk_PreservesExactOffsets(t *testing.T) {
	chunks := Chunk(filing, ChunkOptions{MaxChars: 200, OverlapSentences: 1})
	if len(chunks) < 2 {
		t.Fatalf("expected the text to split, got %d chunk(s)", len(chunks))
	}
	for _, c := range chunks {
		if c.StartChar < 0 || c.EndChar > len(filing) || c.StartChar >= c.EndChar {
			t.Fatalf("chunk %d has invalid offsets %d:%d", c.Index, c.StartChar, c.EndChar)
		}
		if got := filing[c.StartChar:c.EndChar]; got != c.Text {
			t.Fatalf("chunk %d text does not match its own offsets:\n got %q\nwant %q",
				c.Index, c.Text, got)
		}
	}
}

// Every character of the source must appear in at least one chunk, or claims in
// the gap become unextractable.
func TestChunk_CoversTheWholeDocument(t *testing.T) {
	chunks := Chunk(filing, ChunkOptions{MaxChars: 150, OverlapSentences: 1})
	covered := make([]bool, len(filing))
	for _, c := range chunks {
		for i := c.StartChar; i < c.EndChar; i++ {
			covered[i] = true
		}
	}
	for i, ok := range covered {
		if !ok && !isSpaceByte(filing[i]) {
			t.Fatalf("byte %d (%q) is in no chunk", i, string(filing[i]))
		}
	}
}

// Extraction only finds inference edges within a single chunk, so consecutive
// chunks must share text or a premise and its conclusion can never be linked.
//
// The guarantee is conditional: overlap requires the previous chunk to hold at
// least two sentences. When one sentence already fills MaxChars there is
// nothing to step back over. Chunk documents why that case is accepted rather
// than worked around.
func TestChunk_OverlapsSoInferenceEdgesSurvive(t *testing.T) {
	// A fixture of short sentences, so chunks hold several and overlap is
	// actually possible. The `filing` fixture has deliberately long sentences
	// and exercises the degenerate case in the test below.
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString("Party ")
		b.WriteByte(byte('A' + i))
		b.WriteString(" filed a report on the transaction. ")
	}
	text := b.String()

	chunks := Chunk(text, ChunkOptions{MaxChars: 200, OverlapSentences: 2})
	if len(chunks) < 2 {
		t.Fatalf("expected the text to split, got %d chunk(s)", len(chunks))
	}
	for _, c := range chunks {
		if got := text[c.StartChar:c.EndChar]; got != c.Text {
			t.Fatalf("chunk %d offsets do not match its text", c.Index)
		}
	}
	overlapping := 0
	for i := 1; i < len(chunks); i++ {
		if chunks[i-1].Sentences < 2 {
			continue // overlap is structurally impossible here
		}
		if chunks[i].StartChar >= chunks[i-1].EndChar {
			t.Errorf("chunks %d and %d do not overlap (%d >= %d) despite chunk %d "+
				"holding %d sentences; a claim spanning the boundary would lose its "+
				"inference edge",
				i-1, i, chunks[i].StartChar, chunks[i-1].EndChar, i-1, chunks[i-1].Sentences)
		} else {
			overlapping++
		}
	}
	if overlapping == 0 {
		t.Error("no chunk pair overlapped; the overlap mechanism is not exercised")
	}
}

// The degenerate case, asserted explicitly so the limitation is visible rather
// than discovered later: a sentence longer than MaxChars yields single-sentence
// chunks that abut instead of overlapping.
func TestChunk_SingleSentenceChunksCannotOverlap(t *testing.T) {
	chunks := Chunk(filing, ChunkOptions{MaxChars: 100, OverlapSentences: 2})
	if len(chunks) < 2 {
		t.Skip("text did not split")
	}
	for _, c := range chunks {
		if c.Sentences != 1 {
			t.Fatalf("expected every chunk to hold one sentence at MaxChars=100, "+
				"chunk %d holds %d", c.Index, c.Sentences)
		}
	}
	// Forward progress must still hold - no infinite loop, no duplicate chunks.
	for i := 1; i < len(chunks); i++ {
		if chunks[i].StartChar <= chunks[i-1].StartChar {
			t.Fatalf("chunker did not advance: chunk %d starts at %d, chunk %d at %d",
				i-1, chunks[i-1].StartChar, i, chunks[i].StartChar)
		}
	}
}

// The failure that actually matters in filings: splitting on the period inside
// an abbreviation or a decimal fragments a claim.
func TestSplitSentences_DoesNotBreakOnAbbreviationsOrDecimals(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"decimal", "The stake was 60.5% of the total. It rose later.", 2},
		{"us state", "Organised in the U.S. state of Delaware. That is common.", 2},
		{"inc", "Acme Inc. filed the report. Zeta Corp. did not.", 2},
		{"number", "No. 5 Harbour Road is the address. It is registered.", 2},
		{"initial", "Signed by J. Smith on Tuesday. The filing was accepted.", 2},
		{"plain", "One sentence. Two sentences. Three sentences.", 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitSentences(c.text)
			if len(got) != c.want {
				var parts []string
				for _, s := range got {
					parts = append(parts, c.text[s.start:s.end])
				}
				t.Errorf("split into %d sentences, want %d: %q", len(got), c.want, parts)
			}
		})
	}
}

func TestSplitSentences_OffsetsAreContiguous(t *testing.T) {
	ss := splitSentences(filing)
	if len(ss) == 0 {
		t.Fatal("no sentences found")
	}
	if ss[0].start != 0 {
		t.Errorf("first sentence starts at %d, want 0", ss[0].start)
	}
	for i := 1; i < len(ss); i++ {
		if ss[i].start != ss[i-1].end {
			t.Errorf("gap between sentence %d (ends %d) and %d (starts %d)",
				i-1, ss[i-1].end, i, ss[i].start)
		}
	}
	if last := ss[len(ss)-1].end; last != len(filing) {
		t.Errorf("last sentence ends at %d, want %d", last, len(filing))
	}
}

// An overlap wider than the chunk must not loop forever.
func TestChunk_TerminatesWithPathologicalOverlap(t *testing.T) {
	done := make(chan []TextChunk, 1)
	go func() {
		done <- Chunk(filing, ChunkOptions{MaxChars: 80, OverlapSentences: 50})
	}()
	select {
	case chunks := <-done:
		if len(chunks) == 0 {
			t.Fatal("expected chunks")
		}
	default:
		// Give it a moment; the goroutine should finish essentially instantly.
	}
	chunks := Chunk(filing, ChunkOptions{MaxChars: 80, OverlapSentences: 50})
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
}

func TestChunk_EmptyInput(t *testing.T) {
	if got := Chunk("", DefaultChunkOptions()); got != nil {
		t.Errorf("empty text should yield no chunks, got %d", len(got))
	}
	if got := Chunk("   \n\t ", DefaultChunkOptions()); len(got) != 0 {
		t.Errorf("whitespace-only text should yield no chunks, got %d", len(got))
	}
}

func TestChunk_MultiByteText(t *testing.T) {
	text := "Société Générale filed the report. Björk Ríkharðsdóttir signed it."
	chunks := Chunk(text, ChunkOptions{MaxChars: 40, OverlapSentences: 0})
	for _, c := range chunks {
		if got := text[c.StartChar:c.EndChar]; got != c.Text {
			t.Fatalf("multi-byte offsets are wrong:\n got %q\nwant %q", c.Text, got)
		}
	}
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// ─── ids ─────────────────────────────────────────────────────────────────────

// Re-ingesting the same document must MERGE onto the same nodes, not double the
// graph.
func TestIDs_AreDeterministic(t *testing.T) {
	if a, b := DocumentID("https://sec.gov/x"), DocumentID("https://sec.gov/x"); a != b {
		t.Errorf("DocumentID is not stable: %s vs %s", a, b)
	}
	if a, b := ClaimID("d:1", 10, 20, "Zeta held 60%"), ClaimID("d:1", 10, 20, "Zeta held 60%"); a != b {
		t.Errorf("ClaimID is not stable: %s vs %s", a, b)
	}
}

// Trivial formatting differences must canonicalise to one entity, or the graph
// cannot find a path through the company.
func TestEntityID_CanonicalisesFormatting(t *testing.T) {
	want := EntityID("Company", "Zeta Holdings LLC")
	for _, variant := range []string{
		"Zeta Holdings, LLC",
		"  Zeta   Holdings LLC  ",
		"ZETA HOLDINGS LLC",
		"Zeta Holdings  LLC.",
	} {
		if got := EntityID("Company", variant); got != want {
			t.Errorf("EntityID(%q) = %s, want %s", variant, got, want)
		}
	}
}

// Type is part of the key: merging a company with an address of the same name
// would corrupt every traversal through either.
func TestEntityID_SeparatesTypes(t *testing.T) {
	if EntityID("Company", "Acme") == EntityID("Address", "Acme") {
		t.Error("a company and an address with the same name must not share an id")
	}
}

// The same sentence in two filings has different provenance and different
// trust, so the claims must stay distinct.
func TestClaimID_SeparatesByDocument(t *testing.T) {
	a := ClaimID("d:1", 0, 10, "Zeta held 60% of Acme")
	b := ClaimID("d:2", 0, 10, "Zeta held 60% of Acme")
	if a == b {
		t.Error("identical text in different documents must produce different claim ids")
	}
}

// Two claims extracted from the same sentence must not collide.
func TestClaimID_SeparatesBySpan(t *testing.T) {
	a := ClaimID("d:1", 0, 10, "same text")
	b := ClaimID("d:1", 5, 15, "same text")
	if a == b {
		t.Error("different spans must produce different claim ids")
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Zeta Holdings, LLC": "zeta-holdings-llc",
		"  spaced  out  ":    "spaced-out",
		"A/B Testing":        "a-b-testing",
		"---":                "",
		"Ünïcödé Corp":       "ünïcödé-corp",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChunkID_IsPositional(t *testing.T) {
	d := DocumentID("x")
	if ChunkID(d, 0) == ChunkID(d, 1) {
		t.Error("chunk ids must differ by index")
	}
	if !strings.HasPrefix(ChunkID(d, 0), "k:") {
		t.Error("chunk ids should be namespaced")
	}
}
