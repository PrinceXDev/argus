// Command argus-ingest builds the ARGUS claim graph.
//
// It can seed the deterministic synthetic corpus (no network beyond the LLM
// provider), ingest real SEC EDGAR filings, or both. The synthetic corpus is
// what makes the benchmark's abstention and poisoning levels scoreable; EDGAR
// provides real, public-domain filings for credibility and scale.
//
//	argus-ingest -schema-only          create indexes and constraints, write nothing
//	argus-ingest -synthetic            seed the synthetic corpus
//	argus-ingest -synthetic -reset     drop the graph first
//	argus-ingest -edgar-cik 320193     ingest one company's filings from EDGAR
//	argus-ingest -questions out.json   write the benchmark question set
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prince/argus/internal/config"
	"github.com/prince/argus/internal/corpus"
	"github.com/prince/argus/internal/edgar"
	"github.com/prince/argus/internal/extract"
	"github.com/prince/argus/internal/ingest"
	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/llm"
)

func main() {
	var (
		schemaOnly  = flag.Bool("schema-only", false, "create indexes and constraints, then exit")
		synthetic   = flag.Bool("synthetic", false, "seed the deterministic synthetic corpus")
		reset       = flag.Bool("reset", false, "delete the graph before ingesting")
		seed        = flag.Int64("seed", 42, "synthetic corpus seed")
		edgarCIK    = flag.String("edgar-cik", "", "SEC CIK to ingest (comma-separated for several)")
		edgarForms  = flag.String("edgar-forms", "SC 13D,SC 13D/A,4,3", "EDGAR form types to fetch")
		edgarLimit  = flag.Int("edgar-limit", 10, "maximum EDGAR filings per company")
		questions   = flag.String("questions", "", "write the benchmark question set to this path and exit")
		concurrency = flag.Int("concurrency", 4, "parallel extraction calls")
		verbose     = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Writing the question set needs no database and no API key, so it is
	// handled before any connection is attempted.
	if *questions != "" {
		if err := writeQuestions(*questions, *seed); err != nil {
			fatal(log, err)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, log, runOpts{
		schemaOnly:  *schemaOnly,
		synthetic:   *synthetic,
		reset:       *reset,
		seed:        *seed,
		edgarCIK:    *edgarCIK,
		edgarForms:  *edgarForms,
		edgarLimit:  *edgarLimit,
		concurrency: *concurrency,
	}); err != nil {
		fatal(log, err)
	}
}

type runOpts struct {
	schemaOnly  bool
	synthetic   bool
	reset       bool
	seed        int64
	edgarCIK    string
	edgarForms  string
	edgarLimit  int
	concurrency int
}

func run(ctx context.Context, log *slog.Logger, o runOpts) error {
	cfg, err := config.Load()
	if err != nil {
		// Schema creation needs no model, so a missing API key must not block it.
		if !o.schemaOnly {
			return err
		}
		cfg = config.ForSchemaOnly()
	}

	graph, err := kg.Dial(kg.Options{
		URL:       cfg.FalkorURL,
		GraphName: cfg.GraphName,
		Timeout:   60 * time.Second,
	})
	if err != nil {
		return err
	}
	defer graph.Close()

	if err := graph.Ping(ctx); err != nil {
		return err
	}
	log.Info("connected", "graph", cfg.GraphName)

	if o.reset {
		// Deleting a graph that does not exist is not an error worth stopping
		// for, so the outcome is logged rather than returned.
		if err := graph.DropGraph(ctx, cfg.GraphName); err != nil {
			log.Warn("reset: nothing to drop", "err", err)
		} else {
			log.Info("reset: graph dropped", "graph", cfg.GraphName)
		}
	}

	report, err := kg.EnsureSchema(ctx, graph, cfg.EmbedDim)
	if err != nil {
		return err
	}
	log.Info("schema ready", "created", len(report.Created), "existing", len(report.Existing))
	if o.schemaOnly {
		fmt.Println(report.String())
		return nil
	}

	client, err := llm.NewOpenRouter(llm.OpenRouterOptions{
		APIKey:     cfg.LLMAPIKey,
		ChatModel:  cfg.LLMModel,
		EmbedModel: cfg.EmbedModel,
		EmbedDim:   cfg.EmbedDim,
		Referer:    os.Getenv("OPENROUTER_REFERER"),
		Title:      os.Getenv("OPENROUTER_TITLE"),
	})
	if err != nil {
		return err
	}

	pipeline := ingest.NewPipeline(
		extract.NewExtractor(client),
		ingest.NewWriter(graph, client),
		log,
	)
	pipeline.Concurrency = o.concurrency

	var docs []ingest.RawDocument

	if o.synthetic {
		opts := corpus.DefaultOptions()
		opts.Seed = o.seed
		c := corpus.Build(corpus.Generate(opts), corpus.DefaultPerturbations())
		docs = append(docs, syntheticDocs(c)...)
		log.Info("synthetic corpus generated",
			"seed", o.seed, "documents", len(c.Docs),
			"withheld", len(c.Withheld), "poisoned", len(c.Poisoned),
			"contradicted", len(c.Contradicted), "restated", len(c.Restated))
	}

	if o.edgarCIK != "" {
		if cfg.EdgarUserAgent == "" {
			return fmt.Errorf("EDGAR_USER_AGENT is required to fetch from the SEC " +
				"(they reject requests without a contact email)")
		}
		fetcher := edgar.New(cfg.EdgarUserAgent, log)
		fetched, err := fetcher.FetchCompanies(ctx, edgar.Request{
			CIKs:  splitCSV(o.edgarCIK),
			Forms: splitCSV(o.edgarForms),
			Limit: o.edgarLimit,
		})
		if err != nil {
			return err
		}
		docs = append(docs, fetched...)
		log.Info("edgar fetched", "documents", len(fetched))
	}

	if len(docs) == 0 {
		return fmt.Errorf("nothing to ingest: pass -synthetic and/or -edgar-cik")
	}

	results, err := pipeline.IngestAll(ctx, docs)
	summary := ingest.Summarise(results)
	// The summary is printed even on failure: knowing how far a run got is more
	// useful than knowing only that it stopped.
	fmt.Println(summary.String())
	if err != nil {
		return err
	}
	return nil
}

// syntheticDocs adapts generated documents to the ingest pipeline's input.
func syntheticDocs(c *corpus.Corpus) []ingest.RawDocument {
	out := make([]ingest.RawDocument, 0, len(c.Docs))
	for _, d := range c.Docs {
		out = append(out, ingest.RawDocument{
			URL:         d.URL,
			Title:       d.Title,
			Text:        d.Text,
			PublishedAt: d.PublishedAt,
			SHA256:      d.SHA256(),
			Source: ingest.Source{
				Name:  d.SourceName,
				Kind:  d.SourceKind,
				Trust: d.SourceTrust,
			},
		})
	}
	return out
}

func writeQuestions(path string, seed int64) error {
	opts := corpus.DefaultOptions()
	opts.Seed = seed
	c := corpus.Build(corpus.Generate(opts), corpus.DefaultPerturbations())
	qs := c.Questions()

	b, err := json.MarshalIndent(qs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %d questions to %s\n", len(qs.Questions), path)
	for _, s := range qs.Counts() {
		fmt.Printf("  %s\n", s)
	}
	return nil
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if v := trim(s[start:i]); v != "" {
				out = append(out, v)
			}
			start = i + 1
		}
	}
	return out
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func fatal(log *slog.Logger, err error) {
	log.Error("ingest failed", "err", err)
	os.Exit(1)
}
