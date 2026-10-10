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
)

type Server struct {
	httpServer *http.Server
	logger     *slog.Logger
}

func NewServer(cfg config.Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	collector := metrics.NewCollector(cfg.CollectionTimeout, cfg.DiskPath)
	return &Server{
		httpServer: &http.Server{
			Addr:              cfg.ListenAddress,
			Handler:           api.NewHandler(collector, cfg.RequestTimeout, logger),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       cfg.RequestTimeout,
			WriteTimeout:      cfg.RequestTimeout,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    16 * 1024,
		},
		logger: logger,
	}
}

func (s *Server) Run(ctx context.Context) error {
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
