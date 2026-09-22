package extract

import (
	"fmt"
	"strings"
	"unicode"
)

// Span is a half-open character range into a chunk's text.
type Span struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

func (s Span) String() string { return fmt.Sprintf("%d:%d", s.Start, s.End) }

// Valid reports whether the span is well-formed and inside a text of length n.
func (s Span) Valid(n int) bool {
	return s.Start >= 0 && s.End > s.Start && s.End <= n
}

// FindSpan locates a quote inside source text and returns its character range.
//
// This is the anti-hallucination guard, and it is deliberately strict before it
// is lenient. A model that fabricates a fact must also fabricate a quote, and a
// fabricated quote does not survive a substring search against the document it
// claims to come from. Rejecting here - rather than flagging downstream - means
// an unsupported claim never becomes a node, so it can never appear on a proof
// chain at all.
//
// Three passes, in decreasing strictness:
//
//  1. Exact substring. The overwhelming majority of honest quotes match here.
//  2. Whitespace-normalised. Models routinely collapse newlines and runs of
//     spaces when quoting from a PDF or an HTML filing; that is a formatting
//     artefact, not a fabrication, so it is matched back to real offsets.
//  3. Nothing else. Fuzzy matching is explicitly not attempted: an edit-distance
//     match would let a model quietly alter a number or negate a sentence and
//     still pass, which is exactly the attack this guard exists to stop.
func FindSpan(text, quote string) (Span, bool) {
	quote = strings.TrimSpace(quote)
	if quote == "" || text == "" {
		return Span{}, false
	}

	// Pass 1: exact.
	if i := strings.Index(text, quote); i >= 0 {
		return Span{Start: i, End: i + len(quote)}, true
	}

	// Pass 2: whitespace-normalised. Build a normalised copy of the text along
	// with a map back to original byte offsets, so a match yields real offsets
	// into the untouched source rather than into the normalised copy.
	norm, offsets := normaliseWithOffsets(text)
	nq := normaliseSpace(quote)
	if nq == "" {
		return Span{}, false
	}
	i := strings.Index(norm, nq)
	if i < 0 {
		return Span{}, false
	}
	// offsets[i] is the original byte offset of normalised byte i. The end maps
	// from the last matched byte so the span covers it inclusively.
	last := i + len(nq) - 1
	if i >= len(offsets) || last >= len(offsets) {
		return Span{}, false
	}
	start := offsets[i]
	end := offsets[last] + 1
	if start >= end || end > len(text) {
		return Span{}, false
	}
	return Span{Start: start, End: end}, true
}

// normaliseSpace collapses every run of whitespace to a single space.
func normaliseSpace(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// normaliseWithOffsets collapses whitespace runs while recording, for each byte
// of the normalised string, the byte offset it came from in the original.
func normaliseWithOffsets(s string) (string, []int) {
	var b strings.Builder
	b.Grow(len(s))
	offsets := make([]int, 0, len(s))

	inSpace := false
	started := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isSpaceByte(c) {
			inSpace = true
			continue
		}
		if inSpace && started {
			b.WriteByte(' ')
			offsets = append(offsets, i)
		}
		inSpace = false
		started = true
		b.WriteByte(c)
		offsets = append(offsets, i)
	}
	return b.String(), offsets
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// Validated is an extraction that has passed every integrity check, with graph
// IDs not yet assigned.
type Validated struct {
	Entities  []ValidEntity
	Claims    []ValidClaim
	Entails   []ValidEntail
	Relations []ValidRelation
}

type ValidEntity struct {
	Name string
	Type EntityType
	Span Span
}

type ValidClaim struct {
	Ref        string
	Text       string
	Span       Span
	Entities   []string
	Confidence float64
	ValidFrom  string
	ValidTo    string
}

type ValidEntail struct {
	From       string
	To         string
	Kind       EdgeKind
	Confidence float64 // calibrated
	Leap       int64
	Rationale  string
}

type ValidRelation struct {
	From   string
	To     string
	Type   RelationType
	Amount float64
	Span   Span
}

// Validate enforces every integrity rule against the chunk the extraction came
// from, returning what survived plus the reason for each rejection.
//
// The rules, in order of application:
//
//   - An entity must have a name, a known type, and a quote present in the text.
//   - A claim must have text, a quote present in the text, and at least one
//     entity that survived validation. A claim about nobody cannot be anchored
//     to, so it is unreachable by the proof engine and only adds noise.
//   - An entailment must connect two claims that both survived, must not be a
//     self-loop, and gets its confidence calibrated and its leap cost assigned
//     from its kind.
//   - A relation must connect two surviving entities and carry a real quote.
func Validate(text string, x Extraction) (Validated, Report) {
	var out Validated
	rep := Report{ChunksProcessed: 1}

	// --- entities ---
	keptEntities := map[string]ValidEntity{}
	for _, e := range x.Entities {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			rep.Rejections = append(rep.Rejections, Rejection{
				Kind: "empty", Detail: "entity has no name"})
			continue
		}
		if !e.Type.Valid() {
			rep.Rejections = append(rep.Rejections, Rejection{
				Kind: "type", Detail: fmt.Sprintf("unknown entity type %q for %q", e.Type, name)})
			continue
		}
		span, ok := FindSpan(text, e.Quote)
		if !ok {
			rep.Rejections = append(rep.Rejections, Rejection{
				Kind:   "span",
				Detail: fmt.Sprintf("entity %q quote not found in source", name),
				Quote:  e.Quote,
			})
			continue
		}
		if _, dup := keptEntities[name]; dup {
			continue
		}
		keptEntities[name] = ValidEntity{Name: name, Type: e.Type, Span: span}
	}
	for _, e := range keptEntities {
		out.Entities = append(out.Entities, e)
	}
	rep.EntitiesKept = len(out.Entities)

	// --- claims ---
	keptClaims := map[string]bool{}
	for _, c := range x.Claims {
		ref := strings.TrimSpace(c.Ref)
		body := strings.TrimSpace(c.Text)
		if ref == "" || body == "" {
			rep.Rejections = append(rep.Rejections, Rejection{
				Kind: "empty", Detail: "claim has no ref or no text", Quote: c.Quote})
			continue
		}
		span, ok := FindSpan(text, c.Quote)
		if !ok {
			rep.Rejections = append(rep.Rejections, Rejection{
				Kind:   "span",
				Detail: fmt.Sprintf("claim %q quote not found in source", ref),
				Quote:  c.Quote,
			})
			continue
		}
		var ents []string
		for _, n := range c.Entities {
			n = strings.TrimSpace(n)
			if _, ok := keptEntities[n]; ok {
				ents = append(ents, n)
			}
		}
		if len(ents) == 0 {
			rep.Rejections = append(rep.Rejections, Rejection{
				Kind:   "entity",
				Detail: fmt.Sprintf("claim %q names no entity that survived validation", ref),
				Quote:  c.Quote,
			})
			continue
		}
		if keptClaims[ref] {
			rep.Rejections = append(rep.Rejections, Rejection{
				Kind: "ref", Detail: fmt.Sprintf("duplicate claim ref %q", ref)})
			continue
		}
		keptClaims[ref] = true

		// A claim's own confidence is capped at the "stated" ceiling: it was read
		// directly from text, so it can be near-certain, but never certain.
		conf := KindStated.Calibrate(c.Confidence)
		out.Claims = append(out.Claims, ValidClaim{
			Ref: ref, Text: body, Span: span, Entities: ents,
			Confidence: conf, ValidFrom: c.ValidFrom, ValidTo: c.ValidTo,
		})
	}
	rep.ClaimsKept = len(out.Claims)

	// --- entailments ---
	for _, e := range x.Entails {
		from, to := strings.TrimSpace(e.From), strings.TrimSpace(e.To)
		if !keptClaims[from] || !keptClaims[to] {
			rep.Rejections = append(rep.Rejections, Rejection{
				Kind:   "ref",
				Detail: fmt.Sprintf("entailment %s->%s references a claim that did not survive", from, to),
			})
			continue
		}
		if from == to {
			rep.Rejections = append(rep.Rejections, Rejection{
				Kind: "ref", Detail: fmt.Sprintf("self-entailment on %s", from)})
			continue
		}
		kind := e.Kind
		if !kind.Valid() {
			// Unknown kinds are demoted, not dropped: keeping the edge at
			// assumption strength preserves the derivation while pricing the
			// uncertainty honestly.
			kind = KindAssumption
		}
		out.Entails = append(out.Entails, ValidEntail{
			From: from, To: to, Kind: kind,
			Confidence: kind.Calibrate(e.Confidence),
			Leap:       kind.LeapCost(),
			Rationale:  strings.TrimSpace(e.Rationale),
		})
	}
	rep.EntailsKept = len(out.Entails)

	// --- relations ---
	for _, r := range x.Relations {
		from, to := strings.TrimSpace(r.From), strings.TrimSpace(r.To)
		if _, ok := keptEntities[from]; !ok {
			continue
		}
		if _, ok := keptEntities[to]; !ok {
			continue
		}
		if from == to || !r.Type.Valid() {
			continue
		}
		span, ok := FindSpan(text, r.Quote)
		if !ok {
			rep.Rejections = append(rep.Rejections, Rejection{
				Kind:   "span",
				Detail: fmt.Sprintf("relation %s-[%s]->%s quote not found", from, r.Type, to),
				Quote:  r.Quote,
			})
			continue
		}
		out.Relations = append(out.Relations, ValidRelation{
			From: from, To: to, Type: r.Type, Amount: r.Amount, Span: span,
		})
	}
	rep.RelationsKept = len(out.Relations)

	return out, rep
}
