// Command argus-bench runs the ARGUS benchmark and writes the report.
//
//	argus-bench                       run every arm, write bench/results.md
//	argus-bench -arms A1-vector,A5-argus   run a subset
//	argus-bench -limit 20             a quick smoke run
//	argus-bench -json bench/raw.json  keep the per-question outcomes
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prince/argus/internal/answer"
	"github.com/prince/argus/internal/config"
	"github.com/prince/argus/internal/corpus"
	"github.com/prince/argus/internal/court"
	"github.com/prince/argus/internal/eval"
	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/llm"
	"github.com/prince/argus/internal/prove"
)

func main() {
	var (
		out     = flag.String("out", "bench/results.md", "markdown report path")
		rawOut  = flag.String("json", "", "optional path for per-question outcomes")
		seed    = flag.Int64("seed", 42, "corpus seed; must match the ingested corpus")
		limit   = flag.Int("limit", 0, "run only the first N questions (0 = all)")
		armList = flag.String("arms", "", "comma-separated arm names (default: all)")
		verbose = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, log, *out, *rawOut, *seed, *limit, *armList); err != nil {
		log.Error("benchmark failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, out, rawOut string, seed int64, limit int, armList string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// The benchmark reads. It never writes, so it takes a read-only handle and
	// cannot disturb the corpus it is measuring.
	graph, err := kg.Dial(kg.Options{
		URL:       cfg.FalkorURL,
		GraphName: cfg.GraphName,
		Timeout:   60 * time.Second,
		ReadOnly:  true,
	})
	if err != nil {
		return err
	}
	defer graph.Close()
	if err := graph.Ping(ctx); err != nil {
		return err
	}

	client, err := llm.NewOpenRouter(llm.OpenRouterOptions{
		APIKey:     cfg.LLMAPIKey,
		ChatModel:  cfg.LLMModel,
		EmbedModel: cfg.EmbedModel,
		EmbedDim:   cfg.EmbedDim,
	})
	if err != nil {
		return err
	}

	// The question set is regenerated from the seed rather than read from disk,
	// so it cannot drift from the corpus that was ingested. A mismatched seed
	// would silently score against the wrong world.
	opts := corpus.DefaultOptions()
	opts.Seed = seed
	c := corpus.Build(corpus.Generate(opts), corpus.DefaultPerturbations())
	qs := c.Questions()
	if limit > 0 && limit < len(qs.Questions) {
		qs.Questions = qs.Questions[:limit]
	}
	log.Info("question set", "seed", seed, "questions", len(qs.Questions))
	for _, s := range qs.Counts() {
		log.Info("  " + s)
	}

	prover := prove.NewEngine(graph, client)
	all := []eval.Arm{
		eval.NewKeywordArm(graph, client),
		eval.NewVectorArm(graph, client),
		eval.NewHybridArm(graph, client),
		eval.NewGraphNaiveArm(graph, client),
		eval.NewArgusArm(prover, court.NewEngine(graph), answer.NewWriter(client), graph),
	}
	arms, err := selectArms(all, armList)
	if err != nil {
		return err
	}

	report, err := eval.NewRunner(client, log).Run(ctx, qs, arms, cfg.LLMModel)
	if report == nil {
		return err
	}

	if err := writeReport(out, report); err != nil {
		return err
	}
	if rawOut != "" {
		if err := writeJSON(rawOut, report); err != nil {
			return err
		}
	} else {
		// Per-question outcomes are large; they are kept out of the markdown
		// unless explicitly requested.
		report.Outcomes = nil
	}

	fmt.Println()
	fmt.Println(report.Markdown())
	// A run interrupted partway still writes what it has, and reports why.
	return err
}

func selectArms(all []eval.Arm, list string) ([]eval.Arm, error) {
	if strings.TrimSpace(list) == "" {
		return all, nil
	}
	want := map[string]bool{}
	for _, n := range strings.Split(list, ",") {
		want[strings.TrimSpace(n)] = true
	}
	var out []eval.Arm
	for _, a := range all {
		if want[a.Name()] {
			out = append(out, a)
			delete(want, a.Name())
		}
	}
	if len(want) > 0 {
		var unknown, known []string
		for n := range want {
			unknown = append(unknown, n)
		}
		for _, a := range all {
			known = append(known, a.Name())
		}
		return nil, fmt.Errorf("unknown arm(s) %s; available: %s",
			strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	return out, nil
}

func writeReport(path string, r *eval.Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(r.Markdown()), 0o644)
}

func writeJSON(path string, r *eval.Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
