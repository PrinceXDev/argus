package kg

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Mode declares whether a template mutates the graph.
type Mode int

const (
	ModeRead Mode = iota
	ModeWrite
)

func (m Mode) String() string {
	if m == ModeWrite {
		return "write"
	}
	return "read"
}

// Template is a named, parameterised Cypher query.
//
// ARGUS never builds Cypher by concatenating user input. Every query in the
// system is declared here as a compile-time constant, and the only thing a
// request can influence is the value of a $parameter. This is a deliberate
// rejection of the text-to-Cypher pattern common in GraphRAG systems: an LLM
// that emits executable query text is a remote-code-execution surface, and
// FalkorDB's own retrieval benchmark measures text-to-Cypher as worth +0.4%
// accuracy for +1.5s latency, which does not buy the risk.
//
// Where a question genuinely needs a choice of query shape, the LLM selects a
// Template by name from a fixed registry and fills typed parameters. It never
// authors Cypher.
type Template struct {
	// Name identifies the template in logs, metrics and errors.
	Name string
	// Cypher is the query text. It must reference every value as a $parameter.
	Cypher string
	// Mode declares read or write intent, checked against the client's own mode.
	Mode Mode
	// Params lists the parameter names the query requires. render fails if any
	// is missing, so a typo produces a clear error rather than a silent null.
	Params []string
	// Doc explains what the query is for. Surfaced in the query catalogue that
	// ships in the README, since the hackathon requires submitting the Cypher
	// the product depends on.
	Doc string
}

// render validates the parameter set and returns the query text. It does not
// interpolate: FalkorDB receives the parameters separately, which is what makes
// injection structurally impossible rather than merely filtered.
func (t Template) render(params Params) (string, error) {
	if t.Cypher == "" {
		return "", fmt.Errorf("kg: template %q has no Cypher", t.Name)
	}
	var missing []string
	for _, p := range t.Params {
		if _, ok := params[p]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("kg: template %q missing parameters: %s",
			t.Name, strings.Join(missing, ", "))
	}
	return t.Cypher, nil
}

// writeClause matches any Cypher clause that can mutate the graph or the
// schema. Used by Validate to catch a template whose declared Mode disagrees
// with what it actually does.
var writeClause = regexp.MustCompile(
	`(?i)\b(CREATE|MERGE|DELETE|DETACH|SET|REMOVE|DROP|LOAD\s+CSV)\b`)

// paramRef matches a $parameter reference.
var paramRef = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)

// Validate self-checks a template. Called for every registered template at
// startup so a mistake fails the process rather than a request.
func (t Template) Validate() error {
	if t.Name == "" {
		return fmt.Errorf("kg: template with empty name")
	}
	if t.Cypher == "" {
		return fmt.Errorf("kg: template %q has no Cypher", t.Name)
	}

	body := stripComments(t.Cypher)

	if t.Mode == ModeRead && writeClause.MatchString(body) {
		return fmt.Errorf("kg: template %q is declared read-only but contains a write clause", t.Name)
	}

	// Every $parameter the query references must be declared, so the missing
	// parameter check in render is complete rather than best-effort.
	declared := make(map[string]bool, len(t.Params))
	for _, p := range t.Params {
		declared[p] = true
	}
	seen := make(map[string]bool)
	for _, m := range paramRef.FindAllStringSubmatch(body, -1) {
		name := m[1]
		seen[name] = true
		if !declared[name] {
			return fmt.Errorf("kg: template %q references $%s but does not declare it in Params", t.Name, name)
		}
	}
	for _, p := range t.Params {
		if !seen[p] {
			return fmt.Errorf("kg: template %q declares parameter %q that the query never uses", t.Name, p)
		}
	}
	return nil
}

// stripComments removes // line comments and block comments so the validator is
// not fooled by a write keyword that only appears in prose.
func stripComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "//") {
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				break
			}
			i += j
			continue
		}
		if strings.HasPrefix(s[i:], "/*") {
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				break
			}
			i += 2 + j + 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// registry holds every template known to ARGUS, keyed by name.
var registry = map[string]Template{}

// register adds a template to the catalogue and panics if it is invalid or
// duplicated. Called from package-level var blocks, so failures surface at
// process start.
func register(t Template) Template {
	if err := t.Validate(); err != nil {
		panic(err)
	}
	if _, dup := registry[t.Name]; dup {
		panic(fmt.Errorf("kg: duplicate template name %q", t.Name))
	}
	registry[t.Name] = t
	return t
}

// Lookup returns a template by name. This is the only way an LLM-selected query
// shape can reach the database.
func Lookup(name string) (Template, bool) {
	t, ok := registry[name]
	return t, ok
}

// Catalogue returns every registered template, sorted by name. Used to generate
// the query reference in the README.
func Catalogue() []Template {
	out := make([]Template, 0, len(registry))
	for _, t := range registry {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
