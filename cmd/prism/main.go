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

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/observability"
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

	// The server starts here in M1.
	logger.Info("prism started", "version", version)
	<-ctx.Done()
	logger.Info("shutting down")
	return 0
}
