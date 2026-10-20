// Command prism is an HTTP reverse proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/observability"
	"github.com/MuditGarg007/prism/internal/runtime"
	"github.com/MuditGarg007/prism/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("prism", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "prism.yaml", "path to the config file")
	validateOnly := fs.Bool("validate", false, "validate the config and exit")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVersion {
		fmt.Fprintln(stdout, "prism", version)
		return 0
	}

	logger, err := observability.NewLogger(stderr, *logLevel, *logFormat)
	if err != nil {
		fmt.Fprintln(stderr, "prism:", err)
		return 2
	}
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "prism:", err)
		return 1
	}
	// Build everything -validate checks too, as nginx -t does, so that a
	// CA file or certificate that does not parse fails here.
	rt, err := runtime.Build(cfg, logger)
	if err != nil {
		fmt.Fprintln(stderr, "prism:", err)
		return 1
	}
	defer rt.Close()
	cur := runtime.NewCurrent(rt)
	srv, err := server.New(cfg.Listeners, cfg.ReadyPath, cur, logger)
	if err != nil {
		fmt.Fprintln(stderr, "prism:", err)
		return 1
	}
	if *validateOnly {
		fmt.Fprintf(stdout, "configuration %s is valid\n", *configPath)
		return 0
	}

	logger.Info("config loaded",
		"path", *configPath,
		"listeners", len(cfg.Listeners),
		"upstreams", len(cfg.Upstreams),
		"routes", len(cfg.Routes))

	// Room for two signals: the first starts a graceful shutdown, and a
	// second one cuts it short.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	if err := srv.Start(); err != nil {
		logger.Error("cannot listen", "error", err)
		return 1
	}
	rt.Start()
	logger.Info("prism started", "version", version)

	code := 0
	select {
	case sig := <-sigs:
		logger.Info("shutting down", "signal", sig.String(),
			"drain_delay", cfg.Shutdown.DrainDelay, "timeout", cfg.Shutdown.Timeout)
	case err := <-srv.Err():
		logger.Error("listener failed", "error", err)
		code = 1
	}
	if err := shutdown(srv, cfg.Shutdown, sigs, logger); err != nil {
		logger.Warn("shutdown did not finish cleanly", "error", err)
		code = 1
	}
	return code
}

// errInterrupted is a shutdown cut short by a second signal.
var errInterrupted = errors.New("interrupted by a second signal")

// shutdown drains srv: it keeps serving for the drain delay while reporting
// not ready, then stops accepting connections and gives active requests up
// to the timeout. A signal on sigs during either phase closes everything at
// once.
func shutdown(srv *server.Server, sd config.Shutdown, sigs <-chan os.Signal, log *slog.Logger) error {
	srv.Drain()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go func() {
		select {
		case <-sigs:
			log.Warn("second signal, closing all connections now")
			cancel(errInterrupted)
		case <-ctx.Done():
		}
	}()

	if sd.DrainDelay > 0 {
		t := time.NewTimer(sd.DrainDelay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
		}
	}
	tctx, tcancel := context.WithTimeout(ctx, sd.Timeout)
	defer tcancel()
	err := srv.Shutdown(tctx)
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}
