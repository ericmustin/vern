package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/engine/elastic"
	"github.com/ericmustin/vern/internal/redact"
	"github.com/ericmustin/vern/internal/samplescore"
	"github.com/ericmustin/vern/internal/server"
	"github.com/ericmustin/vern/web"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	serveMaxRunTimeout = 10 * time.Minute
	// serveSweepMargin keeps the janitor's age threshold clear of the longest
	// possible run, so it never deletes a run in progress.
	serveSweepMargin = time.Minute
	// serveShutdownGrace is added to --run-timeout for graceful shutdown, so
	// an in-flight run can finish and purge its data.
	serveShutdownGrace = 10 * time.Second
	// serveReadyBackoffMax caps the wait between background Ready attempts.
	serveReadyBackoffMax = 30 * time.Second
	// defaultListen is vern serve's listen address, and so what `vern
	// healthcheck` probes when VERN_LISTEN_ADDR is unset.
	defaultListen = ":8080"
	// healthcheckTimeout covers the probe; a loaded host can answer slowly.
	healthcheckTimeout = 10 * time.Second
)

// serveReadyBackoffStart is the first wait between background Ready
// attempts; it doubles up to serveReadyBackoffMax. A variable so tests can
// shrink it.
var serveReadyBackoffStart = time.Second

// serveFlags holds vern serve's settings. Each flag defaults from its env var
// (see serveEnv), so the same image runs anywhere.
type serveFlags struct {
	listen        string
	maxConcurrent int
	runTimeout    time.Duration
	sweepAge      time.Duration
	sweepEvery    time.Duration
	setupTemplate bool
	readyTimeout  time.Duration

	hsts bool
}

// serveEnv maps flag names to the env vars they default from.
var serveEnv = []struct{ flag, env string }{
	{"listen", "VERN_LISTEN_ADDR"},
	{"max-concurrent", "VERN_MAX_CONCURRENT_RUNS"},
	{"run-timeout", "VERN_RUN_TIMEOUT"},
	{"sweep-age", "VERN_SWEEP_AGE"},
	{"sweep-every", "VERN_SWEEP_EVERY"},
	{"setup-template", "VERN_SETUP_TEMPLATE"},
	{"hsts", "VERN_HSTS"},
}

func (o *serveFlags) register(f *pflag.FlagSet) {
	f.StringVar(&o.listen, "listen", defaultListen, "address to listen on, host:port (env VERN_LISTEN_ADDR)")
	f.IntVar(&o.maxConcurrent, "max-concurrent", 1, "maximum concurrent score runs; more get 429. Above 1 needs an Elasticsearch heap over 1 GiB: with the sensitive-data rules, two concurrent runs trip a 1 GiB node's circuit breaker (env VERN_MAX_CONCURRENT_RUNS)")
	f.DurationVar(&o.runTimeout, "run-timeout", 120*time.Second, "deadline for one score run, at most 10m (env VERN_RUN_TIMEOUT)")
	f.DurationVar(&o.sweepAge, "sweep-age", 10*time.Minute, "the janitor deletes leftover runs older than this; at least --run-timeout + 1m (env VERN_SWEEP_AGE)")
	f.DurationVar(&o.sweepEvery, "sweep-every", 5*time.Minute, "how often the janitor sweeps (env VERN_SWEEP_EVERY)")
	f.BoolVar(&o.setupTemplate, "setup-template", false, "create the metrics-otel@custom component template if absent; cluster-wide, so only for single-node or ephemeral clusters (env VERN_SETUP_TEMPLATE)")
	f.BoolVar(&o.hsts, "hsts", false, "send Strict-Transport-Security; only behind HTTPS (env VERN_HSTS)")
	f.DurationVar(&o.readyTimeout, "ready-timeout", 90*time.Second, "how long one bootstrap attempt waits for Elasticsearch to answer; serve keeps retrying in the background")
}

// resolve fills flags the command line left unset from their env vars, then
// validates every setting. All problems come back as one joined error.
func (o *serveFlags) resolve(f *pflag.FlagSet, getenv func(string) string) error {
	var errs []error
	for _, e := range serveEnv {
		v := getenv(e.env)
		if v == "" || f.Changed(e.flag) {
			continue
		}
		if err := f.Set(e.flag, v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.env, err))
		}
	}
	return errors.Join(append(errs, o.validate()...)...)
}

func (o *serveFlags) validate() []error {
	var errs []error
	if _, _, err := net.SplitHostPort(o.listen); err != nil {
		errs = append(errs, fmt.Errorf("--listen %q must be host:port (e.g. :8080 or 127.0.0.1:8080)", o.listen))
	}
	if o.maxConcurrent < 1 {
		errs = append(errs, fmt.Errorf("--max-concurrent must be at least 1, got %d", o.maxConcurrent))
	}
	if o.runTimeout <= 0 {
		errs = append(errs, fmt.Errorf("--run-timeout must be positive, got %s", o.runTimeout))
	}
	if o.runTimeout > serveMaxRunTimeout {
		errs = append(errs, fmt.Errorf("--run-timeout %s exceeds the %s maximum", o.runTimeout, serveMaxRunTimeout))
	}
	if min := o.runTimeout + serveSweepMargin; o.sweepAge < min {
		errs = append(errs, fmt.Errorf("--sweep-age %s must be at least --run-timeout + 1m (%s), so the janitor never deletes a run in progress", o.sweepAge, min))
	}
	if o.sweepEvery <= 0 {
		errs = append(errs, fmt.Errorf("--sweep-every must be positive, got %s", o.sweepEvery))
	}
	if o.readyTimeout <= 0 {
		errs = append(errs, fmt.Errorf("--ready-timeout must be positive, got %s", o.readyTimeout))
	}
	return errs
}

var serveOpts serveFlags

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the local web UI and HTTP API that score uploaded OTLP samples",
	Long: `Serves a web UI and an HTTP API (POST /api/v1/runs) that score uploaded OTLP
JSON samples the same way vern score does: each upload is loaded into
Elasticsearch, scored, and deleted. Nothing is stored.

Connection settings come from the same variables as vern score:
VERN_ELASTICSEARCH_URL, VERN_API_KEY or VERN_API_KEY_FILE,
VERN_ELASTICSEARCH_USERNAME and VERN_ELASTICSEARCH_PASSWORD. Every flag
defaults from the env var named in its help.

serve also reads vern.yaml from the working directory (or --config) when one
exists: its filters and exclusions change the scores it reports.

vern serve starts listening even when Elasticsearch is down: /healthz answers
200 at once, and /readyz answers 503 until Elasticsearch is usable. A janitor
deletes leftover runs older than --sweep-age every --sweep-every. On SIGINT or
SIGTERM it stops accepting connections and waits up to --run-timeout + 10s
for in-flight runs to finish and purge.`,
	Args: cobra.NoArgs,
	RunE: runServe,
}

// healthcheckCmd lets the compose healthcheck probe vern serve: the
// distroless image has no shell or curl.
var healthcheckCmd = &cobra.Command{
	Use:    "healthcheck",
	Short:  "Exit 0 if vern serve answers /healthz with 200 on VERN_LISTEN_ADDR (default :8080)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return healthcheck(cmd.Context(), healthcheckURLFor(os.Getenv("VERN_LISTEN_ADDR")))
	},
}

func init() {
	serveOpts.register(serveCmd.Flags())
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(healthcheckCmd)
}

func runServe(cmd *cobra.Command, _ []string) error {
	errs := []error{serveOpts.resolve(cmd.Flags(), os.Getenv)}
	creds, err := elasticCredentials()
	errs = append(errs, err)
	if err == nil {
		if creds.URL == "" {
			errs = append(errs, errors.New("vern serve needs VERN_ELASTICSEARCH_URL (an Elasticsearch 9.5+ cluster; see deploy/compose.yaml)"))
		} else {
			errs = append(errs, checkESURL(creds.URL))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	cfg := config.Default()
	if _, statErr := os.Stat(configPath); statErr == nil {
		if cfg, err = config.Load(configPath); err != nil {
			return err
		}
	}

	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	// The first SIGINT/SIGTERM starts a graceful shutdown; signal handling is
	// then released so a second one terminates immediately.
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }()

	logger := log.New(cmd.ErrOrStderr(), "", log.LstdFlags)
	return serve(ctx, serveOpts, creds, cfg, logger.Printf, serveHooks{})
}

// serveHooks are test seams; production leaves them zero.
type serveHooks struct {
	engine    engine.Engine     // nil: an Elasticsearch engine built from creds
	score     server.ScoreFunc  // nil: samplescore.Score
	listening func(addr string) // called with the bound address once listening
}

// serve runs the HTTP server until ctx is done, then shuts down gracefully.
// It returns nil after a clean shutdown.
func serve(ctx context.Context, o serveFlags, creds esCredentials, cfg *config.Config, logf func(string, ...any), h serveHooks) error {
	eng := h.engine
	if eng == nil {
		eng = elastic.New(elastic.Config{
			URL: creds.URL, APIKey: creds.APIKey, Username: creds.Username, Password: creds.Password,
			Logf:           logf,
			EnsureTemplate: o.setupTemplate,
			ReadyTimeout:   o.readyTimeout,
		})
	}
	var ready atomic.Bool
	handler := server.New(server.Config{
		Engine:        eng,
		Ready:         ready.Load,
		Score:         h.score,
		Options:       samplescore.Options{Config: cfg, Logf: logf},
		MaxConcurrent: o.maxConcurrent,
		RunTimeout:    o.runTimeout,
		Static:        web.FS(),
		HSTS:          o.hsts,
		Version:       version,
		Logf:          logf,
	})

	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", o.listen, err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// The server takes a run slot before reading the upload, so
		// ReadTimeout is what bounds how long a slow (slow-loris) upload can
		// hold that slot. Keep it.
		ReadTimeout:  60 * time.Second,
		WriteTimeout: o.runTimeout + 15*time.Second,
		IdleTimeout:  60 * time.Second,
	}
	logf("vern serve %s listening on %s (elasticsearch %s)", version, ln.Addr(), redact.URL(creds.URL))
	if h.listening != nil {
		h.listening(ln.Addr().String())
	}

	// Background work stops with ctx (or bgCancel); serve waits for it
	// before returning.
	bgCtx, bgCancel := context.WithCancel(ctx)
	defer bgCancel()
	var wg sync.WaitGroup
	wg.Go(func() { waitReady(bgCtx, eng, logf, &ready) })
	wg.Go(func() { server.RunJanitor(bgCtx, eng, o.sweepAge, o.sweepEvery, logf) })

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		bgCancel()
		wg.Wait()
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	grace := o.runTimeout + serveShutdownGrace
	logf("shutting down: waiting up to %s for in-flight runs", grace)
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	// Shutdown leaves in-flight request contexts alone, so their runs finish
	// and purge before it returns.
	shutdownErr := srv.Shutdown(sctx)
	wg.Wait()
	if shutdownErr != nil {
		srv.Close()
		return fmt.Errorf("shutdown: %w", shutdownErr)
	}
	<-serveErr // http.ErrServerClosed
	logf("shut down")
	return nil
}

// waitReady bootstraps the engine in the background, retrying with backoff
// until it succeeds or ctx is done, so /readyz turns 200 once Elasticsearch
// is up, without a restart. It sets ready once the engine answers.
func waitReady(ctx context.Context, eng engine.Engine, logf func(string, ...any), ready *atomic.Bool) {
	backoff := serveReadyBackoffStart
	for {
		err := eng.Ready(ctx)
		if err == nil {
			ready.Store(true)
			logf("elasticsearch is ready")
			return
		}
		if ctx.Err() != nil {
			return
		}
		logf("elasticsearch not ready, retrying in %s: %v", backoff, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, serveReadyBackoffMax)
	}
}

// healthcheckURLFor is the /healthz URL for a listen address: a wildcard or
// empty host means the loopback, which is where the probe runs.
func healthcheckURLFor(listen string) string {
	if listen == "" {
		listen = defaultListen
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = "", strings.TrimPrefix(defaultListen, ":")
	}
	if a, perr := netip.ParseAddr(host); host == "" || (perr == nil && a.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}

// healthcheck GETs url and fails unless it answers 200.
func healthcheck(ctx context.Context, url string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: HTTP %d", resp.StatusCode)
	}
	return nil
}
