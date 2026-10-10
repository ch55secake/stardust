package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ch55secake/stardust/internal/agent"
	"github.com/ch55secake/stardust/internal/config"
)

var version = "dev"

func main() {
	cfg := config.Default()
	showVersion := flag.Bool("version", false, "print the Stardust version")
	flag.StringVar(&cfg.ListenAddress, "listen", cfg.ListenAddress, "TCP listen address")
	flag.DurationVar(&cfg.CollectionTimeout, "collection-timeout", cfg.CollectionTimeout, "maximum time allowed for metric collection")
	flag.DurationVar(&cfg.RequestTimeout, "request-timeout", cfg.RequestTimeout, "maximum time allowed for an HTTP request")
	flag.StringVar(&cfg.DiskPath, "disk-path", cfg.DiskPath, "filesystem path to measure")
	flag.Parse()

	if *showVersion {
		println("stardust " + version)
		return
	}
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := agent.NewServer(cfg, logger).Run(ctx); err != nil {
		logger.Error("stardust stopped with an error", "error", err)
		os.Exit(1)
	}
}
