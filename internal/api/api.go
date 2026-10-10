package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/ch55secake/stardust/internal/metrics"
)

const Version = "v1"

type Handler struct {
	collector      *metrics.Collector
	requestTimeout time.Duration
	logger         *slog.Logger
}

type HealthResponse struct {
	APIVersion string    `json:"api_version"`
	Status     string    `json:"status"`
	Timestamp  time.Time `json:"timestamp"`
}

type MetricsResponse struct {
	APIVersion string           `json:"api_version"`
	Timestamp  time.Time        `json:"timestamp"`
	Data       metrics.Snapshot `json:"data"`
}

type ErrorResponse struct {
	APIVersion string   `json:"api_version"`
	Error      APIError `json:"error"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewHandler(collector *metrics.Collector, requestTimeout time.Duration, logger *slog.Logger) http.Handler {
	if requestTimeout <= 0 {
		requestTimeout = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{collector: collector, requestTimeout: requestTimeout, logger: logger}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/health":
		if r.Method != http.MethodGet {
			h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
			return
		}
		h.health(w)
	case "/api/v1/metrics":
		if r.Method != http.MethodGet {
			h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
			return
		}
		h.metrics(w, r)
	default:
		h.writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
	}
}

func (h *Handler) health(w http.ResponseWriter) {
	h.writeJSON(w, http.StatusOK, HealthResponse{
		APIVersion: Version,
		Status:     "ok",
		Timestamp:  time.Now().UTC(),
	})
}

func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.requestTimeout)
	defer cancel()

	snapshot, err := h.collector.Collect(ctx)
	if err != nil {
		h.logger.Error("collecting metrics", "error", err)
		h.writeError(w, http.StatusServiceUnavailable, "metrics_unavailable", "metrics collection timed out or failed")
		return
	}
	h.writeJSON(w, http.StatusOK, MetricsResponse{
		APIVersion: Version,
		Timestamp:  time.Now().UTC(),
		Data:       snapshot,
	})
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		h.logger.Error("writing JSON response", "error", err)
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, code, message string) {
	h.writeJSON(w, status, ErrorResponse{
		APIVersion: Version,
		Error:      APIError{Code: code, Message: message},
	})
}
