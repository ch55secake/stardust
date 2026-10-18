package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ch55secake/stardust/internal/services"
)

func WithServices(policy *services.Policy, inspector services.Inspector, restarts ...*services.RestartManager) func(*Handler) {
	return func(h *Handler) {
		h.servicePolicy, h.serviceInspector = policy, inspector
		if len(restarts) > 0 {
			h.restartManager = restarts[0]
		}
	}
}

type ServiceCapabilities struct {
	Enabled               bool   `json:"enabled"`
	Supported             bool   `json:"supported"`
	Authentication        string `json:"authentication"`
	Logs                  bool   `json:"logs"`
	Restart               bool   `json:"restart"`
	RestartAuthentication string `json:"restart_authentication,omitempty"`
}

type ServicePage struct {
	Units      []services.Unit `json:"units"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

func WithServiceLogs(policy *services.Policy, reader services.JournalReader) func(*Handler) {
	return func(h *Handler) {
		h.logPolicy, h.journalReader = policy, reader
		h.logSlots = make(chan struct{}, maxConcurrentServiceLogReads)
	}
}

func (h *Handler) serviceRoute(w http.ResponseWriter, r *http.Request) bool {
	capabilities := r.URL.Path == "/api/v1/capabilities"
	list := r.URL.Path == "/api/v1/services"
	logs := strings.HasPrefix(r.URL.Path, "/api/v1/services/") && strings.HasSuffix(r.URL.Path, "/logs")
	detail := strings.HasPrefix(r.URL.Path, "/api/v1/services/") && !logs
	restart := detail && strings.HasSuffix(r.URL.Path, "/restart")
	operationStatus := strings.HasPrefix(r.URL.Path, "/api/v1/service-operations/")
	if !capabilities && !list && !detail && !logs && !operationStatus {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	if restart {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/services/"), "/restart")
		h.serviceRestartRoute(w, r, name)
		return true
	}
	if operationStatus {
		h.restartStatusRoute(w, r, strings.TrimPrefix(r.URL.Path, "/api/v1/service-operations/"))
		return true
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return true
	}
	if capabilities {
		restartEnabled := h.servicePolicy != nil && h.servicePolicy.RestartEnabled() && h.restartManager != nil
		restartAuthentication := ""
		if restartEnabled {
			restartAuthentication = "bearer"
		}
		h.writeJSON(w, http.StatusOK, struct {
			APIVersion string              `json:"api_version"`
			Timestamp  time.Time           `json:"timestamp"`
			Services   ServiceCapabilities `json:"services"`
		}{APIVersion: Version, Timestamp: time.Now().UTC(), Services: ServiceCapabilities{Enabled: h.servicePolicy != nil && h.serviceInspector != nil, Supported: h.serviceInspector != nil && h.serviceInspector.Supported(), Authentication: "bearer", Logs: h.servicePolicy != nil && h.serviceInspector != nil && h.serviceInspector.Supported() && h.logPolicy != nil && h.journalReader != nil && h.journalReader.Supported(), Restart: restartEnabled, RestartAuthentication: restartAuthentication}})
		return true
	}
	if logs {
		h.serviceLogs(w, r)
		return true
	}
	if h.servicePolicy == nil || h.serviceInspector == nil {
		h.writeError(w, http.StatusServiceUnavailable, "services_disabled", "service inventory is disabled; configure a private service policy file")
		return true
	}
	token, authenticated := serviceBearerToken(r)
	if !authenticated || !h.servicePolicy.Authorized(token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="services"`)
		h.writeError(w, http.StatusUnauthorized, "unauthorized", "a valid service read token is required")
		return true
	}
	if !h.serviceInspector.Supported() {
		h.writeError(w, http.StatusNotImplemented, "services_unsupported", "systemd service inventory is unsupported on this host")
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.requestTimeout)
	defer cancel()
	if detail {
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/services/")
		if !services.ValidUnit(name) {
			h.writeError(w, http.StatusBadRequest, "invalid_unit", "a valid .service unit name is required")
			return true
		}
		if !h.servicePolicy.Allows(name) {
			h.serviceError(w, services.ErrForbidden)
			return true
		}
		unit, err := h.serviceInspector.Inspect(ctx, name)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			h.serviceError(w, err)
			return true
		}
		h.writeJSON(w, http.StatusOK, struct {
			APIVersion string        `json:"api_version"`
			Timestamp  time.Time     `json:"timestamp"`
			Data       services.Unit `json:"data"`
		}{APIVersion: Version, Timestamp: time.Now().UTC(), Data: unit})
		return true
	}
	query, err := parseServiceQuery(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return true
	}
	names := h.servicePolicy.Units()
	page := ServicePage{Units: []services.Unit{}}
	// Cursor is the last returned configured name, not a snapshot token. Unit
	// state is live; policy is fixed until restart. Filtering matches names only.
	if query.cursor != "" && !h.servicePolicy.Allows(query.cursor) {
		h.writeError(w, http.StatusBadRequest, "invalid_query", "cursor is not in the configured allowlist")
		return true
	}
	for index, name := range names {
		if name <= query.cursor || !strings.Contains(name, query.filter) {
			continue
		}
		unit, inspectErr := h.serviceInspector.Inspect(ctx, name)
		if inspectErr == nil {
			inspectErr = ctx.Err()
		}
		if errors.Is(inspectErr, services.ErrNotFound) {
			unit = services.Unit{Name: name, ID: name, LoadState: "not-found", ActiveState: "inactive", SubState: "dead"}
		} else if inspectErr != nil {
			h.serviceError(w, inspectErr)
			return true
		}
		page.Units = append(page.Units, unit)
		if len(page.Units) == query.limit {
			for _, remaining := range names[index+1:] {
				if strings.Contains(remaining, query.filter) {
					page.NextCursor = name
					break
				}
			}
			break
		}
	}
	h.writeJSON(w, http.StatusOK, struct {
		APIVersion string      `json:"api_version"`
		Timestamp  time.Time   `json:"timestamp"`
		Data       ServicePage `json:"data"`
	}{APIVersion: Version, Timestamp: time.Now().UTC(), Data: page})
	return true
}

func (h *Handler) serviceRestartRoute(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported for service restart")
		return
	}
	if r.URL.RawQuery != "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "restart requests do not accept query parameters")
		return
	}
	if len(r.Header.Values("Origin")) != 0 {
		h.writeError(w, http.StatusForbidden, "origin_forbidden", "browser-originated restart requests are not supported")
		return
	}
	if h.servicePolicy == nil || !h.servicePolicy.RestartEnabled() {
		h.writeError(w, http.StatusServiceUnavailable, "restart_disabled", "service restart is disabled; configure a private restart policy")
		return
	}
	token, authenticated := serviceBearerToken(r)
	if !authenticated || !h.servicePolicy.RestartAuthorized(token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="service-restart"`)
		h.writeError(w, http.StatusUnauthorized, "unauthorized", "a valid service restart token is required")
		return
	}
	if h.serviceInspector == nil || !h.serviceInspector.Supported() {
		h.writeError(w, http.StatusNotImplemented, "services_unsupported", "systemd service restart is unsupported on this host")
		return
	}
	if h.restartManager == nil {
		h.writeError(w, http.StatusServiceUnavailable, "restart_unavailable", "the service restart operation store is unavailable")
		return
	}
	if !services.ValidRestartUnit(name) {
		h.writeError(w, http.StatusBadRequest, "invalid_unit", "a valid .service unit name is required")
		return
	}
	if !h.servicePolicy.AllowsRestart(name) {
		h.writeError(w, http.StatusForbidden, "unit_forbidden", "unit is not in the configured restart allowlist")
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "restart requests must not contain a body")
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || !services.ValidIdempotencyKey(keys[0]) {
		h.writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "one random Idempotency-Key header is required")
		return
	}
	operation, created, err := h.restartManager.Start(name, keys[0], r.RemoteAddr)
	if err != nil {
		h.restartError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	h.writeJSON(w, status, struct {
		APIVersion string                    `json:"api_version"`
		Timestamp  time.Time                 `json:"timestamp"`
		Data       services.RestartOperation `json:"data"`
	}{APIVersion: Version, Timestamp: time.Now().UTC(), Data: operation})
}

func (h *Handler) restartStatusRoute(w http.ResponseWriter, r *http.Request, operationID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported for restart operation status")
		return
	}
	if r.URL.RawQuery != "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request", "restart status requests do not accept query parameters")
		return
	}
	if h.servicePolicy == nil || !h.servicePolicy.RestartEnabled() {
		h.writeError(w, http.StatusServiceUnavailable, "restart_disabled", "service restart is disabled")
		return
	}
	token, authenticated := serviceBearerToken(r)
	if !authenticated || !h.servicePolicy.RestartAuthorized(token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="service-restart"`)
		h.writeError(w, http.StatusUnauthorized, "unauthorized", "a valid service restart token is required")
		return
	}
	if h.restartManager == nil {
		h.writeError(w, http.StatusServiceUnavailable, "restart_unavailable", "the service restart operation store is unavailable")
		return
	}
	operation, err := h.restartManager.Status(operationID)
	if err != nil {
		h.restartError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, struct {
		APIVersion string                    `json:"api_version"`
		Timestamp  time.Time                 `json:"timestamp"`
		Data       services.RestartOperation `json:"data"`
	}{APIVersion: Version, Timestamp: time.Now().UTC(), Data: operation})
}

func (h *Handler) restartError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, services.ErrForbidden):
		h.writeError(w, http.StatusForbidden, "unit_forbidden", "unit is not in the configured restart allowlist")
	case errors.Is(err, services.ErrUnsupported):
		h.writeError(w, http.StatusNotImplemented, "services_unsupported", "systemd service restart is unsupported on this host")
	case errors.Is(err, services.ErrUnitBusy):
		h.writeError(w, http.StatusConflict, "unit_busy", "a restart operation for this unit is already in progress")
	case errors.Is(err, services.ErrIdempotencyConflict):
		h.writeError(w, http.StatusConflict, "idempotency_conflict", "Idempotency-Key was already used for a different unit")
	case errors.Is(err, services.ErrOperationNotFound):
		h.writeError(w, http.StatusNotFound, "operation_not_found", "restart operation not found or expired")
	case errors.Is(err, services.ErrOperationLimit):
		h.writeError(w, http.StatusServiceUnavailable, "restart_capacity", "restart operation capacity is full; retry later")
	case errors.Is(err, services.ErrOperationStore):
		h.writeError(w, http.StatusServiceUnavailable, "restart_unavailable", "restart operation state could not be persisted")
	default:
		h.writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "one random Idempotency-Key header is required")
	}
}

func serviceBearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

type serviceQuery struct {
	limit          int
	cursor, filter string
}

func parseServiceQuery(r *http.Request) (serviceQuery, error) {
	query := serviceQuery{limit: 20}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return query, errors.New("malformed query parameters")
	}
	for key, entries := range values {
		if len(entries) != 1 {
			return query, errors.New("query parameters must not repeat")
		}
		switch key {
		case "limit":
			limit, err := strconv.Atoi(entries[0])
			if err != nil || limit < 1 || limit > 50 {
				return query, errors.New("limit must be between 1 and 50")
			}
			query.limit = limit
		case "cursor":
			if !services.ValidUnit(entries[0]) {
				return query, errors.New("invalid cursor")
			}
			query.cursor = entries[0]
		case "filter":
			if len(entries[0]) > 255 {
				return query, errors.New("filter is too long")
			}
			query.filter = entries[0]
		default:
			return query, errors.New("unknown query parameter")
		}
	}
	return query, nil
}

func (h *Handler) serviceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, services.ErrForbidden):
		h.writeError(w, http.StatusForbidden, "unit_forbidden", "unit is not in the configured allowlist")
	case errors.Is(err, services.ErrPermissionDenied):
		h.writeError(w, http.StatusServiceUnavailable, "services_permission_denied", "permission to inspect systemd units was denied")
	case errors.Is(err, services.ErrNotFound):
		h.writeError(w, http.StatusNotFound, "unit_not_found", "service unit not found")
	case errors.Is(err, services.ErrUnsupported):
		h.writeError(w, http.StatusNotImplemented, "services_unsupported", services.ErrUnsupported.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		h.writeError(w, http.StatusGatewayTimeout, "services_timeout", "service inspection timed out or was canceled")
	default:
		h.writeError(w, http.StatusServiceUnavailable, "services_unavailable", services.ErrUnavailable.Error())
	}
}
