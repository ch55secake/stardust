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
	"github.com/ch55secake/stardust/internal/services"
)

var version = "dev"

func main() {
	cfg := config.Default()
	showVersion := flag.Bool("version", false, "print the Stardust version")
	servicePolicyFile := flag.String("service-policy-file", "", "private JSON file containing service credentials and exact read/restart allowlists (restart disabled by default)")
	flag.StringVar(&cfg.ListenAddress, "listen", cfg.ListenAddress, "TCP listen address")
	flag.StringVar(&cfg.APITokenFile, "api-token-file", "", "private file containing the optional health/metrics bearer token")
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
	apiToken, err := config.ReadAPITokenFile(cfg.APITokenFile)
	if err != nil {
		slog.Error("invalid API token file", "error", err)
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *servicePolicyFile != "" {
		policy, err := services.LoadPolicy(*servicePolicyFile)
		if err != nil {
			logger.Error("invalid service policy", "error", err)
			os.Exit(2)
		}
		cfg.ServicePolicy = policy
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := agent.NewServerWithAPIToken(cfg, logger, apiToken).Run(ctx); err != nil {
		logger.Error("stardust stopped with an error", "error", err)
		os.Exit(1)
	}
}
