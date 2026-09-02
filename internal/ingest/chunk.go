package ingest

import (
	"strings"
	"unicode"
)

// Chunking strategy.
//
// ARGUS chunks on sentence boundaries with an overlap, and records exact
// character offsets into the original document. The offsets are not a nicety:
// span integrity resolves a claim's quote to a position inside its chunk, and
// the UI highlights the evidence inside the source. A chunker that loses
// offsets would break both.
//
// Overlap matters more here than in a conventional RAG pipeline. Extraction
// finds inference edges *within* a chunk, so two claims split across a boundary
// can never be linked. Overlapping by a couple of sentences means a premise and
// its conclusion usually land in at least one chunk together.

// ChunkOptions configures the chunker.
type ChunkOptions struct {
	// MaxChars is the soft ceiling for a chunk. Sentences are never split, so a
	// single very long sentence may exceed it.
	MaxChars int
	// OverlapSentences is how many trailing sentences are repeated at the start
	// of the next chunk.
	OverlapSentences int
}

// DefaultChunkOptions are tuned for SEC filings: long, dense, heavily nested
// sentences, where a premise and its consequence are usually adjacent.
func DefaultChunkOptions() ChunkOptions {
	return ChunkOptions{MaxChars: 1800, OverlapSentences: 2}
}

// TextChunk is a span of a document with its exact offsets preserved.
type TextChunk struct {
	Index     int
	Text      string
	StartChar int
	EndChar   int
	// Sentences is how many sentences the chunk holds. Overlap with the next
	// chunk is only possible when this is at least 2 - see Chunk.
	Sentences int
}

// Chunk splits text into overlapping, offset-preserving chunks.
func Chunk(text string, opts ChunkOptions) []TextChunk {
	if opts.MaxChars <= 0 {
		opts.MaxChars = DefaultChunkOptions().MaxChars
	}
	if opts.OverlapSentences < 0 {
		opts.OverlapSentences = 0
	}
	sentences := splitSentences(text)
	if len(sentences) == 0 {
		return nil
	}

	var out []TextChunk
	i := 0
	for i < len(sentences) {
		start := sentences[i].start
		end := sentences[i].end
		j := i + 1
		for j < len(sentences) && sentences[j].end-start <= opts.MaxChars {
			end = sentences[j].end
			j++
		}

		out = append(out, TextChunk{
			Index:     len(out),
			Text:      text[start:end],
			StartChar: start,
			EndChar:   end,
			Sentences: j - i,
		})

		if j >= len(sentences) {
			break
		}
		// Step back by the overlap, but always make forward progress.
		//
		// The clamp has a real consequence worth naming: when a chunk holds a
		// single sentence - because that one sentence already fills MaxChars -
		// there is nothing to step back over, and consecutive chunks abut
		// instead of overlapping. An inference edge whose premise and conclusion
		// straddle that boundary is then unrecoverable, because extraction only
		// links claims within one chunk.
		//
		// This is accepted rather than worked around. The alternative - splitting
		// mid-sentence to force an overlap - would hand the extractor a fragment
		// with no verifiable quote, and span integrity would reject everything
		// derived from it. A missed edge costs recall; a broken quote costs
		// correctness. MaxChars should simply be set well above the longest
		// expected sentence; the default of 1800 is roughly 10x a long filing
		// sentence, so single-sentence chunks do not occur in practice.
		next := j - opts.OverlapSentences
		if next <= i {
			next = i + 1
		}
		i = next
	}
	return out
}

type sentence struct{ start, end int }

// splitSentences finds sentence boundaries, returning offsets into the original
// text so nothing is copied and no position information is lost.
//
// This is a heuristic splitter, not a linguistic one. It is tuned to avoid the
// failure that actually matters in filings: breaking on the period inside
// "U.S.", "Inc.", "No. 5" or a decimal number, which would fragment a claim
// across two chunks and make its inference edge unfindable.
func splitSentences(text string) []sentence {
	var out []sentence
	start := 0
	runes := []rune(text)
	// byteAt maps a rune index to its byte offset, so returned spans index the
	// original string correctly for multi-byte text.
	byteAt := make([]int, len(runes)+1)
	{
		b := 0
		for i, r := range runes {
			byteAt[i] = b
			b += len(string(r))
		}
		byteAt[len(runes)] = len(text)
	}

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r != '.' && r != '!' && r != '?' && r != '\n' {
			continue
		}
		if r == '.' && !isTerminalPeriod(runes, i) {
			continue
		}
		// Consume trailing closers and whitespace into this sentence.
		end := i + 1
		for end < len(runes) && (runes[end] == '"' || runes[end] == '\'' || runes[end] == ')' || runes[end] == ']') {
			end++
		}
		for end < len(runes) && unicode.IsSpace(runes[end]) {
			end++
		}
		bs, be := byteAt[start], byteAt[end]
		if strings.TrimSpace(text[bs:be]) != "" {
			out = append(out, sentence{start: bs, end: be})
		}
		start = end
		i = end - 1
	}
	if start < len(runes) {
		bs := byteAt[start]
		if strings.TrimSpace(text[bs:]) != "" {
			out = append(out, sentence{start: bs, end: len(text)})
		}
	}
	return out
}

// commonAbbrev are the tokens whose trailing period is not a sentence end. The
// list is short and domain-specific on purpose: every entry is one that appears
// routinely in SEC filings and would otherwise fragment a claim.
var commonAbbrev = map[string]bool{
	"inc": true, "corp": true, "co": true, "ltd": true, "llc": true, "llp": true,
	"plc": true, "lp": true, "sa": true, "nv": true, "gmbh": true, "ag": true,
	"mr": true, "mrs": true, "ms": true, "dr": true, "jr": true, "sr": true,
	"st": true, "ave": true, "no": true, "vs": true, "approx": true, "est": true,
	"fig": true, "cf": true, "al": true, "etc": true, "eg": true, "ie": true,
}

// isTerminalPeriod reports whether the period at index i ends a sentence.
func isTerminalPeriod(runes []rune, i int) bool {
	// A period between digits is a decimal point: "60.5%".
	if i > 0 && i+1 < len(runes) && unicode.IsDigit(runes[i-1]) && unicode.IsDigit(runes[i+1]) {
		return false
	}
	// A single-letter token before the period is an initial or an acronym
	// segment: "U.S.", "J. Smith".
	if i >= 2 && unicode.IsLetter(runes[i-1]) && !unicode.IsLetter(runes[i-2]) {
		return false
	}
	if i == 1 && unicode.IsLetter(runes[0]) {
		return false
	}

	// Walk back over the preceding word and check it against the abbreviation
	// list.
	j := i - 1
	for j >= 0 && (unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j])) {
		j--
	}
	word := strings.ToLower(string(runes[j+1 : i]))
	if commonAbbrev[word] {
		return false
	}

	// A sentence end must be followed by whitespace or the end of the text.
	// "3.5" and "example.com" therefore do not split.
	if i+1 < len(runes) && !unicode.IsSpace(runes[i+1]) {
		next := runes[i+1]
		if next != '"' && next != '\'' && next != ')' && next != ']' {
			return false
		}
	}
	return true
}
