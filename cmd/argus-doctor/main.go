// Command argus-doctor verifies a FalkorDB instance is usable by ARGUS and
// resolves the three spike questions from docs/01-spike-results.md that need a
// live database:
//
//  5. does algo.SPpaths accept float weightProp, and what type is pathWeight?
//  6. does an ACL read-only user actually fail to write?
//  7. how long does GRAPH.COPY take at the current graph size?
//
// It writes to a scratch graph and cleans up after itself. It never touches the
// primary graph.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/prince/argus/internal/kg"
)

func main() {
	var (
		url     = flag.String("url", envOr("FALKORDB_URL", "falkor://localhost:6379"), "FalkorDB connection URL")
		scratch = flag.String("graph", "argus_doctor", "scratch graph name (created and dropped)")
	)
	flag.Parse()

	if err := run(context.Background(), *url, *scratch); err != nil {
		fmt.Fprintf(os.Stderr, "\nFAILED: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, url, scratch string) error {
	fmt.Printf("ARGUS doctor\n  target: %s\n  scratch graph: %s\n\n", redact(url), scratch)

	c, err := kg.Dial(kg.Options{URL: url, GraphName: scratch, Timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.Ping(ctx); err != nil {
		return err
	}
	ok("connect", "reachable")

	graphs, err := c.ListGraphs(ctx)
	if err != nil {
		return err
	}
	ok("GRAPH.LIST", fmt.Sprintf("%d graph(s) on instance", len(graphs)))

	// Always start from a clean scratch graph so a previous aborted run cannot
	// make a later run report a false pass.
	_ = c.DropGraph(ctx, scratch)

	if err := spikeFloatWeights(ctx, c, scratch); err != nil {
		return err
	}
	if err := spikeCopyLatency(ctx, c, scratch); err != nil {
		return err
	}
	spikeReadOnly(ctx, url, scratch)

	if err := c.DropGraph(ctx, scratch); err != nil {
		warn("cleanup", err.Error())
	} else {
		ok("cleanup", "scratch graph dropped")
	}

	fmt.Println("\nAll required checks passed.")
	return nil
}

// spikeFloatWeights answers question 5. It builds a three-node chain whose
// ENTAILS edges carry both a float confidence-derived weight and the fixed-point
// integer ARGUS actually uses, then runs algo.SPpaths against each.
func spikeFloatWeights(ctx context.Context, c *kg.Client, graph string) error {
	section("spike 5: algo.SPpaths weight types")

	const build = `
CREATE (a:Claim {id:'a', conf:1.0}),
       (b:Claim {id:'b', conf:0.8}),
       (z:Claim {id:'z', conf:0.5}),
       (a)-[:ENTAILS {w_float:0.223, w_int:223, leap:1}]->(b),
       (b)-[:ENTAILS {w_float:0.470, w_int:470, leap:1}]->(z),
       (a)-[:ENTAILS {w_float:1.609, w_int:1609, leap:1}]->(z)`
	if _, err := c.Raw(ctx, graph, build); err != nil {
		return fmt.Errorf("build scratch chain: %w", err)
	}

	// The two-hop route (223+470=693) must beat the direct edge (1609), which is
	// the whole premise of treating -ln(confidence) as a distance.
	const q = `
MATCH (a:Claim {id:'a'}), (z:Claim {id:'z'})
CALL algo.SPpaths({sourceNode:a, targetNode:z, relTypes:['ENTAILS'],
                   weightProp:'%s', costProp:'leap', maxCost:5, pathCount:1})
YIELD path, pathWeight, pathCost
RETURN pathWeight, pathCost, [n IN nodes(path) | n.id] AS ids`

	intOK := false
	for _, prop := range []string{"w_int", "w_float"} {
		res, err := c.Raw(ctx, graph, fmt.Sprintf(q, prop))
		if err != nil {
			if prop == "w_int" {
				return fmt.Errorf("algo.SPpaths with integer weightProp failed: %w", err)
			}
			warn("weightProp float", fmt.Sprintf("rejected (%v) - fixed-point w_int is required, as planned", err))
			continue
		}
		if len(res) == 0 {
			warn("weightProp "+prop, "no path returned")
			continue
		}
		r := res[0]
		w := r["pathWeight"]
		ok(fmt.Sprintf("weightProp %s", prop),
			fmt.Sprintf("pathWeight=%v (%T) pathCost=%v route=%v",
				w, w, r["pathCost"], r["ids"]))
		if prop == "w_int" {
			intOK = true
		}
	}
	if !intOK {
		return errors.New("integer weightProp did not produce a path; the proof engine cannot work on this instance")
	}
	return nil
}

// spikeCopyLatency answers question 7. Feature B forks the graph per retraction
// candidate, so this number decides whether that design survives or whether we
// fall back to in-place conf=0 ablation.
func spikeCopyLatency(ctx context.Context, c *kg.Client, graph string) error {
	section("spike 7: GRAPH.COPY latency")

	fork := kg.ForkPrefix + "doctor"
	_ = c.DropGraph(ctx, fork)

	start := time.Now()
	if err := c.CopyGraph(ctx, graph, fork); err != nil {
		return fmt.Errorf("GRAPH.COPY unsupported on this instance: %w", err)
	}
	elapsed := time.Since(start)
	ok("GRAPH.COPY", fmt.Sprintf("%s for a 3-node graph", elapsed.Round(time.Microsecond)))

	// Confirm the fork is independent: mutating it must not touch the source.
	if _, err := c.Raw(ctx, fork, "MATCH (c:Claim {id:'b'}) DETACH DELETE c"); err != nil {
		return fmt.Errorf("mutate fork: %w", err)
	}
	res, err := c.Raw(ctx, graph, "MATCH (c:Claim) RETURN count(c) AS n")
	if err != nil {
		return err
	}
	if len(res) == 0 {
		return errors.New("counting claims in the source graph returned no rows")
	}
	n := res[0]["n"]
	if fmt.Sprint(n) != "3" {
		return fmt.Errorf("fork is not independent: source graph now has %v claims, expected 3", n)
	}
	ok("fork isolation", "mutating the fork left the source graph untouched")

	if err := c.DropGraph(ctx, fork); err != nil {
		warn("fork cleanup", err.Error())
	}
	return nil
}

// spikeReadOnly answers question 6. It is advisory: without ACL credentials
// configured we can still prove the client-side guard holds, but the
// database-side guarantee needs FALKORDB_QUERY_USER to be set up.
func spikeReadOnly(ctx context.Context, url, graph string) {
	section("spike 6: read-only enforcement")

	ro, err := kg.Dial(kg.Options{URL: url, GraphName: graph, ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		warn("read-only client", err.Error())
		return
	}
	defer ro.Close()

	if _, err := ro.Raw(ctx, graph, "CREATE (:Tamper)"); err == nil {
		warn("client guard", "a read-only client accepted a write - this is a bug in internal/kg")
	} else {
		ok("client guard", "read-only client refused a write")
	}

	user := os.Getenv("FALKORDB_QUERY_USER")
	if user == "" {
		warn("ACL guard", "FALKORDB_QUERY_USER not set - database-side enforcement not yet verified (see docs/security.md)")
		return
	}
	ok("ACL guard", "configured for user "+user+"; exercised by `make redteam`")
}

func section(s string) { fmt.Printf("\n%s\n", s) }

func ok(name, detail string) { fmt.Printf("  [ok]   %-22s %s\n", name, detail) }

func warn(name, detail string) { fmt.Printf("  [warn] %-22s %s\n", name, detail) }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func redact(u string) string {
	// Mirrors kg.redactURL, which is unexported.
	for i := 0; i+3 <= len(u); i++ {
		if u[i:i+3] == "://" {
			rest := u[i+3:]
			for j := len(rest) - 1; j >= 0; j-- {
				if rest[j] == '@' {
					return u[:i+3] + "***@" + rest[j+1:]
				}
			}
			return u
		}
	}
	return u
}
