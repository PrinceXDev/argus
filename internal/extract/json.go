package extract

import (
	"encoding/json"
	"fmt"
	"strings"
)

// decodeJSON parses a model response that should be a bare JSON object.
//
// Schema-constrained decoding usually makes this trivial, but providers
// occasionally wrap the payload in a markdown fence. Stripping that is
// tolerated; anything else is an error, because silently repairing malformed
// model output is how corrupt data gets into a graph.
func decodeJSON(s string, v any) error {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("decoding extraction JSON: %w", err)
	}
	return nil
}
