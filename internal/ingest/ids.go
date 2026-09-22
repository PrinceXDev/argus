// Package ingest turns source documents into the ARGUS claim graph.
package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

// Identifiers are content-derived rather than random.
//
// Every write in ARGUS is a MERGE on an id, so re-ingesting the same document
// must produce the same ids or the graph doubles. Deriving ids from content
// makes ingestion idempotent for free, and makes a re-run after a crash safe
// rather than something to be careful about.
//
// The tradeoff: editing a claim's text changes its id, so the old node is
// orphaned rather than updated. That is the correct behaviour here - a claim
// whose text changed is a different assertion, and silently mutating it would
// rewrite history that a proof chain may already cite.

// SourceID derives a stable id for a publisher.
func SourceID(name string) string { return "s:" + slug(name) }

// DocumentID derives a stable id from a document's canonical location.
func DocumentID(urlOrPath string) string { return "d:" + hash16(urlOrPath) }

// ChunkID derives a stable id from a document and the chunk's position in it.
func ChunkID(documentID string, index int) string {
	return "k:" + strings.TrimPrefix(documentID, "d:") + ":" + itoa(index)
}

// EntityID derives a stable id from an entity's type and canonical name.
//
// Type is part of the key because "Acme" the company and "Acme" the address are
// different things, and merging them would silently corrupt every traversal
// that passes through either.
func EntityID(entityType, canonicalName string) string {
	return "e:" + slug(entityType) + ":" + slug(canonicalName)
}

// ClaimID derives a stable id from the document, the span and the claim text.
//
// All three are needed. Span alone collides when one sentence yields two claims;
// text alone collides when the same sentence appears in two filings, which must
// stay distinct because they have different provenance and different trust.
func ClaimID(documentID string, start, end int, text string) string {
	return "c:" + hash16(documentID+"|"+itoa(start)+":"+itoa(end)+"|"+normalise(text))
}

func hash16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// slug lowercases and reduces a string to [a-z0-9-], collapsing runs.
//
// Canonicalisation is intentionally aggressive: "Zeta Holdings, LLC" and "Zeta
// Holdings LLC" must produce the same id, because they are the same company and
// a graph that treats them as two nodes cannot find a path between them. Deeper
// resolution (fuzzy matching, alias tables) happens later in the pipeline; this
// only removes formatting noise.
func slug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// normalise collapses whitespace and case for hashing, so trivially different
// renderings of the same sentence hash alike.
func normalise(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
