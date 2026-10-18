package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ch55secake/stardust/internal/services"
)

const maxConcurrentServiceLogReads = 4

func (h *Handler) serviceLogs(w http.ResponseWriter, r *http.Request) {
	if h.servicePolicy == nil || h.serviceInspector == nil || h.logPolicy == nil || h.journalReader == nil {
		h.writeError(w, http.StatusServiceUnavailable, "logs_disabled", "service log access is disabled; configure a separate private log read token and unit allowlist")
		return
	}
	token, authenticated := serviceBearerToken(r)
	if !authenticated || !h.logPolicy.Authorized(token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="service-logs"`)
		h.writeError(w, http.StatusUnauthorized, "logs_unauthorized", "a valid service log read token is required")
		return
	}
	if !h.journalReader.Supported() {
		h.writeError(w, http.StatusNotImplemented, "logs_unsupported", services.ErrLogsUnsupported.Error())
		return
	}
	const prefix = "/api/v1/services/"
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/logs")
	if !services.ValidUnit(name) {
		h.writeError(w, http.StatusBadRequest, "invalid_unit", "a valid .service unit name is required")
		return
	}
	if !h.logPolicy.Allows(name) {
		h.writeError(w, http.StatusForbidden, "log_unit_forbidden", "unit is not in the configured log read allowlist")
		return
	}
	query, err := parseLogQuery(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_log_query", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.requestTimeout)
	defer cancel()
	select {
	case h.logSlots <- struct{}{}:
		defer func() { <-h.logSlots }()
	case <-ctx.Done():
		h.logError(w, ctx.Err())
		return
	}
	unit, err := h.serviceInspector.Inspect(ctx, name)
	if err != nil {
		if errors.Is(err, services.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "log_unit_not_found", "service unit not found")
			return
		}
		h.logError(w, err)
		return
	}
	if !services.ValidUnit(unit.ID) {
		h.logError(w, services.ErrLogsUnavailable)
		return
	}
	page, err := h.journalReader.Read(ctx, unit.ID, query)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		h.logError(w, err)
		return
	}
	page.Unit = unit.ID
	response := struct {
		APIVersion string           `json:"api_version"`
		Timestamp  time.Time        `json:"timestamp"`
		Data       services.LogPage `json:"data"`
	}{APIVersion: Version, Timestamp: time.Now().UTC(), Data: page}
	body, err := json.Marshal(response)
	if err != nil || len(body)+1 > services.MaxLogResponseBytes {
		h.writeError(w, http.StatusServiceUnavailable, "logs_unavailable", services.ErrLogsUnavailable.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(append(body, '\n')); err != nil {
		h.logger.Error("writing service log response", "error", err)
	}
}

func parseLogQuery(r *http.Request) (services.LogQuery, error) {
	query := services.LogQuery{Limit: 50}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return query, errors.New("malformed log query parameters")
	}
	for key, entries := range values {
		if len(entries) != 1 {
			return query, errors.New("log query parameters must not repeat")
		}
		switch key {
		case "limit":
			limit, err := strconv.Atoi(entries[0])
			if err != nil || limit < 1 || limit > services.MaxLogEntries {
				return query, errors.New("limit must be between 1 and 100")
			}
			query.Limit = limit
		case "before":
			if len(entries[0]) == 0 || len(entries[0]) > 8192 {
				return query, errors.New("invalid log cursor")
			}
			query.Before = entries[0]
		case "after":
			if len(entries[0]) == 0 || len(entries[0]) > 8192 {
				return query, errors.New("invalid log cursor")
			}
			query.After = entries[0]
		case "since", "until":
			if len(entries[0]) > 64 {
				return query, errors.New("timestamp is too long")
			}
			instant, err := time.Parse(time.RFC3339Nano, entries[0])
			if err != nil {
				return query, errors.New("since/until timestamps must be RFC3339")
			}
			if key == "since" {
				query.Since = instant
			} else {
				query.Until = instant
			}
		default:
			return query, errors.New("unknown log query parameter")
		}
	}
	if query.Before != "" && query.After != "" {
		return query, errors.New("before and after cursors cannot be combined")
	}
	if !query.Since.IsZero() && !query.Until.IsZero() && query.Since.After(query.Until) {
		return query, errors.New("since must not be later than until")
	}
	return query, nil
}

func (h *Handler) logError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, services.ErrLogsForbidden), errors.Is(err, services.ErrForbidden):
		h.writeError(w, http.StatusForbidden, "log_unit_forbidden", "unit is not in the configured log read allowlist")
	case errors.Is(err, services.ErrLogCursorExpired):
		h.writeError(w, http.StatusGone, "log_cursor_expired", "journal cursor expired, rotated, invalidated, or unusable; request recent history again")
	case errors.Is(err, services.ErrLogCursorInvalid):
		h.writeError(w, http.StatusBadRequest, "invalid_log_query", "log cursor is malformed or belongs to another unit")
	case errors.Is(err, services.ErrLogsPermissionDenied), errors.Is(err, services.ErrPermissionDenied):
		h.writeError(w, http.StatusServiceUnavailable, "logs_permission_denied", "Stardust is not authorized to read this unit's journal")
	case errors.Is(err, services.ErrLogsUnsupported), errors.Is(err, services.ErrUnsupported):
		h.writeError(w, http.StatusNotImplemented, "logs_unsupported", services.ErrLogsUnsupported.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		h.writeError(w, http.StatusGatewayTimeout, "logs_timeout", "journal request timed out or was canceled")
	default:
		h.writeError(w, http.StatusServiceUnavailable, "logs_unavailable", services.ErrLogsUnavailable.Error())
	}
}
