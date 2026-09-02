// Package corpus generates a synthetic investigation corpus with known ground
// truth, plus the question set that measures a retrieval system against it.
//
// Why synthesise at all, when real SEC filings are available and free?
//
// Because the three things ARGUS claims to do better than vector RAG cannot be
// measured on a real corpus:
//
//   - Abstention. To score "correctly refuses to answer", the evidence must be
//     provably absent. On a real corpus you can never be sure a fact is not
//     stated somewhere, so a refusal is unscoreable.
//   - Poisoning. To score robustness, the false claim must be known to be false.
//   - Load-bearing facts. To score Feature B, you must know which fact is
//     actually load-bearing, which requires having constructed the derivation.
//
// So the corpus is generated from a ground-truth world, and every question's
// answer is read off that world rather than annotated by hand. Real EDGAR
// filings are ingested alongside it for credibility and scale; the synthetic
// layer is what makes the numbers checkable.
//
// Generation is fully deterministic given a seed. A benchmark you cannot
// regenerate byte-for-byte is not a benchmark.
package corpus

import (
	"fmt"
	"math/rand"
	"time"
)

// Company is a synthetic legal entity.
type Company struct {
	Name         string
	Jurisdiction string
	Address      string
}

// Person is a synthetic individual.
type Person struct {
	Name string
	Role string
}

// Holding is a ground-truth ownership fact.
type Holding struct {
	Holder  string // company name
	Issuer  string // company name
	Percent float64
	From    time.Time
	// To is zero when the holding is current.
	To time.Time
}

// Officer is a ground-truth control fact.
type Officer struct {
	Person  string
	Company string
	Role    string
	From    time.Time
}

// Transfer is a ground-truth transaction.
type Transfer struct {
	From   string
	To     string
	Amount float64
	At     time.Time
}

// World is the ground truth. Everything in the generated documents is derived
// from it, and every question's expected answer is read off it.
type World struct {
	Seed      int64
	Companies []Company
	People    []Person
	Holdings  []Holding
	Officers  []Officer
	Transfers []Transfer

	// Chains records the deliberately constructed multi-hop ownership chains,
	// so multi-hop questions can be generated with a known answer and a known
	// required hop count.
	Chains []Chain
	// Ring is a set of companies wired into a circular transaction structure
	// sharing an address - the fraud-ring case for community detection.
	Ring []string
}

// Chain is an ownership chain: Links[0] ultimately controls Links[len-1].
type Chain struct {
	Links []string
}

// Hops is the number of ownership steps in the chain.
func (c Chain) Hops() int { return len(c.Links) - 1 }

// Ultimate is the entity at the top of the chain.
func (c Chain) Ultimate() string { return c.Links[0] }

// Target is the entity at the bottom.
func (c Chain) Target() string { return c.Links[len(c.Links)-1] }

// Options configures world generation.
type Options struct {
	Seed int64
	// Chains is how many multi-hop ownership chains to build, and how deep.
	Chains   int
	ChainMin int
	ChainMax int
	RingSize int
	Extra    int // standalone companies, as distractors
	BaseYear int
}

// DefaultOptions produce a corpus small enough to ingest in minutes and rich
// enough to separate the retrieval arms.
func DefaultOptions() Options {
	return Options{
		Seed:     42,
		Chains:   14,
		ChainMin: 3,
		ChainMax: 5,
		RingSize: 6,
		Extra:    20,
		BaseYear: 2017,
	}
}

// Generate builds a deterministic ground-truth world.
func Generate(opts Options) *World {
	if opts.Seed == 0 {
		opts.Seed = DefaultOptions().Seed
	}
	if opts.ChainMin < 2 {
		opts.ChainMin = 2
	}
	if opts.ChainMax < opts.ChainMin {
		opts.ChainMax = opts.ChainMin
	}
	rng := rand.New(rand.NewSource(opts.Seed))
	w := &World{Seed: opts.Seed}

	naming := newNamer(rng)

	// --- ownership chains -----------------------------------------------------
	// Each chain is a stack of holding companies ending in an operating company.
	// Every link is a majority stake, so "ultimately controls" is derivable by
	// composing "holds >50%" with "a >50% holder controls the issuer".
	for i := 0; i < opts.Chains; i++ {
		depth := opts.ChainMin + rng.Intn(opts.ChainMax-opts.ChainMin+1)
		var links []string
		for d := 0; d < depth; d++ {
			c := naming.company(rng, d == depth-1)
			w.Companies = append(w.Companies, c)
			links = append(links, c.Name)
		}
		for d := 0; d < depth-1; d++ {
			// Between 51 and 100 percent: always controlling, never unanimous, so
			// the definitional step is always required and never trivial.
			pct := 51 + rng.Float64()*49
			w.Holdings = append(w.Holdings, Holding{
				Holder:  links[d],
				Issuer:  links[d+1],
				Percent: round1(pct),
				From:    dateIn(rng, opts.BaseYear, opts.BaseYear+2),
			})
		}
		w.Chains = append(w.Chains, Chain{Links: links})

		// An officer at the top of the chain, so a question can travel from a
		// named person all the way down to an operating company.
		p := naming.person(rng)
		w.People = append(w.People, p)
		w.Officers = append(w.Officers, Officer{
			Person:  p.Name,
			Company: links[0],
			Role:    p.Role,
			From:    dateIn(rng, opts.BaseYear, opts.BaseYear+1),
		})
	}

	// --- fraud ring -----------------------------------------------------------
	// A closed loop of transfers between companies sharing a registered address.
	// No single document describes the ring; it exists only as graph structure,
	// which is precisely what community detection is for and what a passage
	// retriever cannot see.
	if opts.RingSize >= 3 {
		shared := naming.address(rng)
		var ring []string
		for i := 0; i < opts.RingSize; i++ {
			c := naming.company(rng, false)
			c.Address = shared
			w.Companies = append(w.Companies, c)
			ring = append(ring, c.Name)
		}
		for i := range ring {
			w.Transfers = append(w.Transfers, Transfer{
				From:   ring[i],
				To:     ring[(i+1)%len(ring)],
				Amount: round1(50_000 + rng.Float64()*950_000),
				At:     dateIn(rng, opts.BaseYear+1, opts.BaseYear+3),
			})
		}
		w.Ring = ring
	}

	// --- distractors ----------------------------------------------------------
	// Unconnected companies with plausible names and filings. Without them a
	// retriever can succeed by returning almost anything, and the benchmark
	// stops discriminating.
	for i := 0; i < opts.Extra; i++ {
		c := naming.company(rng, rng.Intn(2) == 0)
		w.Companies = append(w.Companies, c)
		p := naming.person(rng)
		w.People = append(w.People, p)
		w.Officers = append(w.Officers, Officer{
			Person: p.Name, Company: c.Name, Role: p.Role,
			From: dateIn(rng, opts.BaseYear, opts.BaseYear+3),
		})
	}

	return w
}

// CompanyByName returns a company, or false.
func (w *World) CompanyByName(name string) (Company, bool) {
	for _, c := range w.Companies {
		if c.Name == name {
			return c, true
		}
	}
	return Company{}, false
}

// OfficerOf returns the officer record for a company, or false.
func (w *World) OfficerOf(company string) (Officer, bool) {
	for _, o := range w.Officers {
		if o.Company == company {
			return o, true
		}
	}
	return Officer{}, false
}

// HoldingOf returns the holding where issuer is held, or false.
func (w *World) HoldingOf(issuer string) (Holding, bool) {
	for _, h := range w.Holdings {
		if h.Issuer == issuer {
			return h, true
		}
	}
	return Holding{}, false
}

// ─── naming ──────────────────────────────────────────────────────────────────

// Names are drawn from fixed word lists rather than a faker library, so the
// corpus is reproducible from the seed alone with no external dependency and no
// risk of colliding with a real company.

type namer struct {
	used map[string]bool
}

func newNamer(_ *rand.Rand) *namer { return &namer{used: map[string]bool{}} }

var (
	prefixes = []string{
		"Meridian", "Halcyon", "Northgate", "Silverpine", "Cobalt", "Thornbury",
		"Kestrel", "Vantage", "Ironwood", "Blackwater", "Larkspur", "Emberline",
		"Windermere", "Ashcroft", "Foxglove", "Calder", "Stonebridge", "Marlowe",
		"Highfield", "Rookwood", "Sablecrest", "Fenwick", "Dunmore", "Ravensmoor",
	}
	holdingSuffix = []string{
		"Holdings LLC", "Capital Partners LP", "Group Ltd", "Ventures LLC",
		"Investments SA", "Trust NV", "Partners LP", "Equity Holdings Ltd",
	}
	operatingSuffix = []string{
		"Logistics Inc", "Manufacturing Corp", "Technologies Inc", "Marine Services Ltd",
		"Energy Corp", "Distribution Inc", "Chemical Works Ltd", "Freight Corp",
	}
	jurisdictions = []string{
		"Delaware", "Nevada", "Cyprus", "Luxembourg", "British Virgin Islands",
		"Isle of Man", "Singapore", "Malta",
	}
	firstNames = []string{
		"Dana", "Ruth", "Elias", "Nadia", "Owen", "Priya", "Tomas", "Ingrid",
		"Marcus", "Leah", "Farid", "Colette", "Jonas", "Amara", "Viktor", "Rosalind",
	}
	lastNames = []string{
		"Reyes", "Okafor", "Lindqvist", "Baptiste", "Moreau", "Ashworth", "Novak",
		"Delacroix", "Halvorsen", "Whitmore", "Castellanos", "Bergstrom", "Ferreira",
	}
	roles = []string{
		"Chief Executive Officer", "Chief Financial Officer", "Managing Director",
		"Sole Director", "Chairman", "General Partner",
	}
	streets = []string{
		"Harbour Road", "Kingsway", "Old Mill Lane", "Pembroke Street",
		"Victoria Quay", "Cathedral Close",
	}
)

func (n *namer) company(rng *rand.Rand, operating bool) Company {
	suffixes := holdingSuffix
	if operating {
		suffixes = operatingSuffix
	}
	for attempt := 0; ; attempt++ {
		name := prefixes[rng.Intn(len(prefixes))] + " " + suffixes[rng.Intn(len(suffixes))]
		if attempt > 200 {
			// Exhausted the combination space; disambiguate rather than loop.
			name = fmt.Sprintf("%s %d", name, attempt)
		}
		if n.used[name] {
			continue
		}
		n.used[name] = true
		return Company{
			Name:         name,
			Jurisdiction: jurisdictions[rng.Intn(len(jurisdictions))],
			Address:      n.address(rng),
		}
	}
}

func (n *namer) person(rng *rand.Rand) Person {
	for attempt := 0; ; attempt++ {
		name := firstNames[rng.Intn(len(firstNames))] + " " + lastNames[rng.Intn(len(lastNames))]
		if attempt > 200 {
			name = fmt.Sprintf("%s %d", name, attempt)
		}
		if n.used[name] {
			continue
		}
		n.used[name] = true
		return Person{Name: name, Role: roles[rng.Intn(len(roles))]}
	}
}

func (n *namer) address(rng *rand.Rand) string {
	return fmt.Sprintf("%d %s", 1+rng.Intn(200), streets[rng.Intn(len(streets))])
}

func dateIn(rng *rand.Rand, fromYear, toYear int) time.Time {
	y := fromYear + rng.Intn(toYear-fromYear+1)
	return time.Date(y, time.Month(1+rng.Intn(12)), 1+rng.Intn(28), 0, 0, 0, 0, time.UTC)
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
