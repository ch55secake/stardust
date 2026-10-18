package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ch55secake/stardust/internal/metrics"
	"github.com/ch55secake/stardust/internal/services"
)

const Version = "v1"

type Handler struct {
	collector        MetricsCollector
	requestTimeout   time.Duration
	logger           *slog.Logger
	servicePolicy    *services.Policy
	serviceInspector services.Inspector
	apiTokenHash     [sha256.Size]byte
	apiAuthEnabled   bool
}

type MetricsCollector interface {
	Collect(context.Context) (metrics.Snapshot, error)
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

func NewHandler(collector MetricsCollector, requestTimeout time.Duration, logger *slog.Logger, options ...func(*Handler)) http.Handler {
	return newHandler(collector, requestTimeout, logger, "", options...)
}

// NewHandlerWithToken enables bearer authentication for health and metrics
// when apiToken is non-empty. The token is hashed and not retained by the handler.
func NewHandlerWithToken(collector MetricsCollector, requestTimeout time.Duration, logger *slog.Logger, apiToken string, options ...func(*Handler)) http.Handler {
	return newHandler(collector, requestTimeout, logger, apiToken, options...)
}

func newHandler(collector MetricsCollector, requestTimeout time.Duration, logger *slog.Logger, apiToken string, options ...func(*Handler)) http.Handler {
	if requestTimeout <= 0 {
		requestTimeout = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	handler := &Handler{collector: collector, requestTimeout: requestTimeout, logger: logger}
	if apiToken != "" {
		handler.apiTokenHash = sha256.Sum256([]byte(apiToken))
		handler.apiAuthEnabled = true
	}
	for _, option := range options {
		option(handler)
	}
	return handler
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.serviceRoute(w, r) {
		return
	}
	switch r.URL.Path {
	case "/api/v1/health":
		w.Header().Set("Cache-Control", "no-store")
		if !h.authorize(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
			return
		}
		h.health(w)
	case "/api/v1/metrics":
		w.Header().Set("Cache-Control", "no-store")
		if !h.authorize(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
			return
		}
		h.metrics(w, r)
	default:
		h.writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
	}
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) bool {
	if !h.apiAuthEnabled {
		return true
	}
	values := r.Header.Values("Authorization")
	if len(values) == 1 {
		scheme, token, found := strings.Cut(values[0], " ")
		if found && strings.EqualFold(scheme, "Bearer") && token != "" && !strings.ContainsAny(token, " \t\r\n") {
			hash := sha256.Sum256([]byte(token))
			if subtle.ConstantTimeCompare(hash[:], h.apiTokenHash[:]) == 1 {
				return true
			}
		}
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="stardust"`)
	h.writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
	return false
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
