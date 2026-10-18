package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ch55secake/stardust/internal/metrics"
)

type fixtureCollector struct {
	snapshot metrics.Snapshot
	err      error
	collect  func(context.Context) (metrics.Snapshot, error)
}

func (c fixtureCollector) Collect(ctx context.Context) (metrics.Snapshot, error) {
	if c.collect != nil {
		return c.collect(ctx)
	}
	return c.snapshot, c.err
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMetricsReturnsCompleteFixture(t *testing.T) {
	want := metrics.Snapshot{
		Hostname: "server-01",
		OS: metrics.OSInfo{
			Name:            "linux",
			Platform:        "ubuntu",
			PlatformVersion: "24.04",
			KernelArch:      "x86_64",
		},
		CPU: metrics.CPU{
			UsagePercent:        12.5,
			LogicalCores:        4,
			PerCoreUsagePercent: []float64{10, 15, 20, 5},
			LoadAverage:         &metrics.LoadAverage{OneMinute: 0.5, FiveMinutes: 0.75, FifteenMinutes: 1.25},
		},
		Memory: metrics.Memory{UsedBytes: 1000, TotalBytes: 2000, UsedPercent: 50},
		Swap:   metrics.Swap{TotalBytes: 1000, UsedBytes: 300, FreeBytes: 700, UsedPercent: 30},
		Disk:   metrics.Disk{Path: "/", UsedBytes: 1000, TotalBytes: 2000, UsedPercent: 50},
		Filesystems: []metrics.Filesystem{{
			Mountpoint:     "/",
			Device:         "/dev/root",
			FilesystemType: "ext4",
			UsedBytes:      1000,
			TotalBytes:     2000,
			AvailableBytes: 900,
			UsedPercent:    50,
		}},
		Uptime: 86400,
		Network: metrics.Network{
			BytesReceived:   1000,
			BytesSent:       500,
			PacketsReceived: 20,
			PacketsSent:     10,
			Interfaces: []metrics.NetworkInterface{
				{Name: "eth0", BytesReceived: 1000, BytesSent: 500, PacketsReceived: 20, PacketsSent: 10},
			},
		},
	}
	handler := NewHandler(fixtureCollector{snapshot: want}, time.Second, testLogger())
	before := time.Now().UTC()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	assertJSONContentType(t, recorder)
	var response MetricsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.APIVersion != Version {
		t.Fatalf("api version = %q, want %q", response.APIVersion, Version)
	}
	if response.Timestamp.Before(before) || response.Timestamp.After(time.Now().UTC()) {
		t.Fatalf("timestamp = %s, want current UTC time", response.Timestamp)
	}
	if !reflect.DeepEqual(response.Data, want) {
		t.Fatalf("data = %+v, want %+v", response.Data, want)
	}
}

func TestMetricsIncludesZeroSwapWhenNoneIsConfigured(t *testing.T) {
	handler := NewHandler(fixtureCollector{}, time.Second, testLogger())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(response["data"], &data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	encodedSwap, ok := data["swap"]
	if !ok {
		t.Fatal("response data is missing the swap object")
	}
	var got metrics.Swap
	if err := json.Unmarshal(encodedSwap, &got); err != nil {
		t.Fatalf("decode swap: %v", err)
	}
	if got != (metrics.Swap{}) {
		t.Fatalf("swap = %+v, want zero values when swap is not configured", got)
	}
}

func TestMetricsReturnsAvailableDataAndErrors(t *testing.T) {
	want := metrics.Snapshot{
		Hostname: "server-01",
		Errors: []metrics.MetricError{
			{Metric: "disk", Message: "permission denied"},
			{Metric: "swap", Message: "swap metrics unavailable"},
		},
	}
	handler := NewHandler(fixtureCollector{snapshot: want}, time.Second, testLogger())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response MetricsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Data.Hostname != want.Hostname {
		t.Fatalf("hostname = %q, want %q", response.Data.Hostname, want.Hostname)
	}
	if !reflect.DeepEqual(response.Data.Errors, want.Errors) {
		t.Fatalf("errors = %+v, want %+v", response.Data.Errors, want.Errors)
	}
}

func TestMetricsTimeoutUsesErrorSchema(t *testing.T) {
	handler := NewHandler(fixtureCollector{collect: func(ctx context.Context) (metrics.Snapshot, error) {
		<-ctx.Done()
		return metrics.Snapshot{}, ctx.Err()
	}}, 10*time.Millisecond, testLogger())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)

	handler.ServeHTTP(recorder, request)

	assertErrorResponse(t, recorder, http.StatusServiceUnavailable, "metrics_unavailable")
}

func TestMetricsCollectionErrorUsesErrorSchema(t *testing.T) {
	handler := NewHandler(fixtureCollector{err: context.Canceled}, time.Second, testLogger())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)

	handler.ServeHTTP(recorder, request)

	assertErrorResponse(t, recorder, http.StatusServiceUnavailable, "metrics_unavailable")
}

func TestHealth(t *testing.T) {
	handler := NewHandler(fixtureCollector{}, time.Second, testLogger())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	assertJSONContentType(t, recorder)
	var response HealthResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.APIVersion != Version || response.Status != "ok" {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestOptionalBearerAuthenticationProtectsHealthAndMetrics(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	collectionCalls := 0
	handler := NewHandlerWithToken(fixtureCollector{collect: func(context.Context) (metrics.Snapshot, error) {
		collectionCalls++
		return metrics.Snapshot{}, nil
	}}, time.Second, testLogger(), token)

	for _, path := range []string{"/api/v1/health", "/api/v1/metrics"} {
		for _, authorization := range [][]string{
			nil,
			{"Bearer wrong-token"},
			{"Basic " + token},
			{"Bearer " + token, "Bearer " + token},
		} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			for _, value := range authorization {
				request.Header.Add("Authorization", value)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			assertErrorResponse(t, recorder, http.StatusUnauthorized, "unauthorized")
			if recorder.Header().Get("WWW-Authenticate") == "" || recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("missing security response headers for %s: %v", path, recorder.Header())
			}
			if strings.Contains(recorder.Body.String(), token) {
				t.Fatal("credential leaked in unauthorized response")
			}
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "bEaReR "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("valid credential rejected for %s: %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
	if collectionCalls != 1 {
		t.Fatalf("metrics collector called %d times; unauthorized requests must not collect", collectionCalls)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/metrics", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	assertErrorResponse(t, recorder, http.StatusUnauthorized, "unauthorized")
	request.Header.Set("Authorization", "Bearer "+token)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	assertErrorResponse(t, recorder, http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestAPIAuthenticationRemainsOptIn(t *testing.T) {
	for _, path := range []string{"/api/v1/health", "/api/v1/metrics"} {
		recorder := httptest.NewRecorder()
		NewHandler(fixtureCollector{}, time.Second, testLogger()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("legacy unauthenticated request to %s returned %d", path, recorder.Code)
		}
	}
}

func TestUnsupportedMethodsUseErrorSchema(t *testing.T) {
	handler := NewHandler(fixtureCollector{}, time.Second, testLogger())
	for _, path := range []string{"/api/v1/health", "/api/v1/metrics"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, path, nil)

			handler.ServeHTTP(recorder, request)

			assertErrorResponse(t, recorder, http.StatusMethodNotAllowed, "method_not_allowed")
			if got, want := recorder.Header().Get("Allow"), http.MethodGet; got != want {
				t.Errorf("Allow header = %q, want %q", got, want)
			}
		})
	}
}

func TestUnknownEndpointUsesErrorSchema(t *testing.T) {
	handler := NewHandler(fixtureCollector{}, time.Second, testLogger())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/unknown", nil)

	handler.ServeHTTP(recorder, request)

	assertErrorResponse(t, recorder, http.StatusNotFound, "not_found")
}

func assertJSONContentType(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if got, want := recorder.Header().Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Fatalf("content type = %q, want %q", got, want)
	}
}

func assertErrorResponse(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status = %d, want %d", recorder.Code, wantStatus)
	}
	assertJSONContentType(t, recorder)
	var response ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.APIVersion != Version {
		t.Fatalf("api version = %q, want %q", response.APIVersion, Version)
	}
	if response.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q", response.Error.Code, wantCode)
	}
}
