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

// shutdownTimeout bounds how long active requests get to finish on
// shutdown before their connections are closed.
const shutdownTimeout = 15 * time.Second

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
	srv, err := server.New(cfg.Listeners, cur, logger)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Start(); err != nil {
		logger.Error("cannot listen", "error", err)
		return 1
	}
	logger.Info("prism started", "version", version)

	code := 0
	select {
	case <-ctx.Done():
	case err := <-srv.Err():
		logger.Error("listener failed", "error", err)
		code = 1
	}
	logger.Info("shutting down", "timeout", shutdownTimeout)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		logger.Warn("shutdown did not finish cleanly", "error", err)
	}
	return code
}
