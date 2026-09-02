// Command argus-api serves the ARGUS HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prince/argus/internal/answer"
	"github.com/prince/argus/internal/api"
	"github.com/prince/argus/internal/config"
	"github.com/prince/argus/internal/court"
	"github.com/prince/argus/internal/kg"
	"github.com/prince/argus/internal/llm"
	"github.com/prince/argus/internal/prove"
	"github.com/prince/argus/internal/retract"
)

func main() {
	var (
		addr      = flag.String("addr", "", "listen address (default :PORT from the environment)")
		noRetract = flag.Bool("no-retract", false, "disable counterfactual retraction")
		fixture   = flag.Bool("fixture", false, "serve recorded data: no database, no API key")
		verbose   = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *fixture {
		if err := runFixture(log, *addr); err != nil {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
		return
	}

	if err := run(log, *addr, !*noRetract); err != nil {
		log.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, addr string, retractEnabled bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if addr == "" {
		addr = ":" + cfg.Port
	}

	// Two handles to the same instance, with different capabilities.
	//
	// The read handle is what every request path gets, and it is read-only both
	// at the client (ReadOnly: true) and at the database (GRAPH.RO_QUERY, plus
	// an ACL user where one is configured). The write handle exists solely so
	// Feature B can fork, and is never reachable from a query.
	reader, err := kg.Dial(kg.Options{
		URL:       cfg.QueryURL(),
		GraphName: cfg.GraphName,
		Timeout:   30 * time.Second,
		ReadOnly:  true,
	})
	if err != nil {
		return err
	}
	defer reader.Close()

	if err := reader.Ping(context.Background()); err != nil {
		return err
	}
	log.Info("connected", "graph", cfg.GraphName, "readonly", true)

	var forker *kg.Client
	if retractEnabled {
		forker, err = kg.Dial(kg.Options{
			URL:       cfg.FalkorURL,
			GraphName: cfg.GraphName,
			Timeout:   90 * time.Second,
		})
		if err != nil {
			return err
		}
		defer forker.Close()

		// A crashed run leaves counterfactual graphs resident in memory. They
		// are namespaced precisely so they can be found and removed without a
		// registry, and startup is the natural place to do it.
		if n, err := retract.ReapForks(context.Background(), forker); err != nil {
			log.Warn("fork reaper failed", "err", err)
		} else if n > 0 {
			log.Info("reaped orphaned counterfactual graphs", "count", n)
		}
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

	prover := prove.NewEngine(reader, client)
	srv := api.New(api.Config{
		Graph:    reader,
		Forker:   forkerOrNil(forker),
		Prover:   prover,
		Court:    court.NewEngine(reader),
		Retract:  retractOrNil(forker, prover),
		Writer:   answer.NewWriter(client),
		Embedder: client,
		Log:      log,
	})

	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.Routes(),
		// ReadHeaderTimeout guards against slowloris. WriteTimeout is
		// deliberately absent: the ask endpoint streams, and a write deadline
		// would cut a long generation off mid-answer. Per-request deadlines are
		// applied in the handlers instead, where they can be sized to the work.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "retraction", retractEnabled)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	// Reap again on the way out, so a restart does not inherit this run's forks.
	if forker != nil {
		if n, _ := retract.ReapForks(shutdownCtx, forker); n > 0 {
			log.Info("reaped counterfactual graphs on shutdown", "count", n)
		}
	}
	return nil
}

// forkerOrNil avoids handing the API a typed-nil interface, which would be
// non-nil when compared and would make RetractEnabled wrongly true.
func forkerOrNil(c *kg.Client) kg.Writer {
	if c == nil {
		return nil
	}
	return c
}

func retractOrNil(c *kg.Client, p *prove.Engine) *retract.Engine {
	if c == nil {
		return nil
	}
	return retract.NewEngine(c, p)
}

// runFixture serves recorded data.
//
// Every layer above the graph client and the model provider runs for real: the
// proof engine, the evidence lookup, the abstention logic, the answer writer
// and the SSE framing are all the production code paths. Only the two external
// dependencies are recorded, which makes this useful for frontend work and for
// a demo that must not depend on a provider staying up.
//
// The warning below is not decoration. A viewer must always be able to tell
// which mode produced a result, so it is printed on startup and /api/health
// reports fixture:true.
func runFixture(log *slog.Logger, addr string) error {
	if addr == "" {
		addr = ":8080"
	}
	log.Warn("FIXTURE MODE - serving recorded data, not a live graph")

	graph, model := api.NewFixture(8)

	srv := api.New(api.Config{
		Graph:    graph,
		Prover:   prove.NewEngine(graph, model),
		Court:    court.NewEngine(graph),
		Writer:   answer.NewWriter(model),
		Embedder: model,
		Log:      log,
	})
	srv.Fixture = true

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "mode", "fixture")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
