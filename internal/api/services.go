package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ch55secake/stardust/internal/services"
)

func WithServices(policy *services.Policy, inspector services.Inspector) func(*Handler) {
	return func(h *Handler) { h.servicePolicy, h.serviceInspector = policy, inspector }
}

type ServiceCapabilities struct {
	Enabled        bool   `json:"enabled"`
	Supported      bool   `json:"supported"`
	Authentication string `json:"authentication"`
	Logs           bool   `json:"logs"`
	Restart        bool   `json:"restart"`
}

type ServicePage struct {
	Units      []services.Unit `json:"units"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

func (h *Handler) serviceRoute(w http.ResponseWriter, r *http.Request) bool {
	capabilities := r.URL.Path == "/api/v1/capabilities"
	list := r.URL.Path == "/api/v1/services"
	detail := strings.HasPrefix(r.URL.Path, "/api/v1/services/")
	if !capabilities && !list && !detail {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return true
	}
	if capabilities {
		h.writeJSON(w, http.StatusOK, struct {
			APIVersion string              `json:"api_version"`
			Services   ServiceCapabilities `json:"services"`
		}{Version, ServiceCapabilities{Enabled: h.servicePolicy != nil && h.serviceInspector != nil, Supported: h.serviceInspector != nil && h.serviceInspector.Supported(), Authentication: "bearer"}})
		return true
	}
	if h.servicePolicy == nil || h.serviceInspector == nil {
		h.writeError(w, http.StatusServiceUnavailable, "services_disabled", "service inventory is disabled; configure a private service policy file")
		return true
	}
	if len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || !h.servicePolicy.Authorized(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
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
		if err != nil {
			h.serviceError(w, err)
			return true
		}
		h.writeJSON(w, http.StatusOK, struct {
			APIVersion string        `json:"api_version"`
			Data       services.Unit `json:"data"`
		}{Version, unit})
		return true
	}
	query, err := parseServiceQuery(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return true
	}
	names := h.servicePolicy.Units()
	page := ServicePage{Units: []services.Unit{}}
	// Cursor is the last examined configured name, not a snapshot token. Unit
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
		Data       ServicePage `json:"data"`
	}{Version, page})
	return true
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
