package agent

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/ch55secake/stardust/internal/api"
	"github.com/ch55secake/stardust/internal/config"
	"github.com/ch55secake/stardust/internal/metrics"
	"github.com/ch55secake/stardust/internal/services"
)

type Server struct {
	httpServer     *http.Server
	logger         *slog.Logger
	restartManager *services.RestartManager
}

func NewServer(cfg config.Config, logger *slog.Logger) *Server {
	return NewServerWithAPIToken(cfg, logger, "")
}

func NewServerWithAPIToken(cfg config.Config, logger *slog.Logger, apiToken string) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	collector := metrics.NewCollector(cfg.CollectionTimeout, cfg.DiskPath)
	systemd := services.NewSystemd()
	var restartManager *services.RestartManager
	if cfg.ServicePolicy != nil && cfg.ServicePolicy.RestartEnabled() && systemd.Supported() {
		manager, err := services.NewRestartManager(services.DefaultRestartStorePath, cfg.ServicePolicy, systemd, systemd, logger)
		if err != nil {
			logger.Error("service restart is disabled because its operation store is unavailable", "error", err)
		} else {
			restartManager = manager
		}
	}
	return &Server{
		httpServer: &http.Server{
			Addr:              cfg.ListenAddress,
			Handler:           api.NewHandlerWithToken(collector, cfg.RequestTimeout, logger, apiToken, api.WithServices(cfg.ServicePolicy, systemd, restartManager), api.WithServiceLogs(cfg.ServicePolicy.LogReadPolicy(), services.NewJournal())),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       cfg.RequestTimeout,
			WriteTimeout:      cfg.RequestTimeout,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    16 * 1024,
		},
		logger:         logger,
		restartManager: restartManager,
	}
}

func (s *Server) Run(ctx context.Context) error {
	if s.restartManager != nil {
		defer s.restartManager.Close()
	}
	listener, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}
	s.logger.Info("stardust listening", "address", s.httpServer.Addr)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.httpServer.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.logger.Info("stardust shutting down", "reason", ctx.Err())
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	}
}
