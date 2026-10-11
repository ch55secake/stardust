# Stardust

Stardust is a lightweight Linux telemetry agent for Orbit. It runs without
root privileges, keeps no persistent state, and binds to loopback by default
so Orbit can reach it through an SSH port forward.

## Features

- CPU, memory, disk, uptime, network, hostname, and operating system metrics
- Small versioned JSON API using only the Go standard HTTP server
- Configurable collection and request timeouts
- Graceful shutdown and useful lifecycle logging
- Hardened systemd unit in `deploy/systemd/stardust.service`

## Install

On Linux, install the latest release with one command (the default destination
is `/usr/local/bin`):

```sh
curl -fsSL https://raw.githubusercontent.com/ch55secake/stardust/main/install.sh | sudo bash
```

The installer detects `amd64` or `arm64`, verifies the release archive against
the published SHA-256 checksum, and installs the `stardust` binary. To install
without root privileges, choose a writable directory on your `PATH`:

```sh
curl -fsSL https://raw.githubusercontent.com/ch55secake/stardust/main/install.sh | bash -s -- --install-dir "$HOME/.local/bin"
```

To install a specific release, add `--version vX.Y.Z` to the command.

## Build and run

Requirements for building from source: Go 1.26 or newer.

```sh
make build
./bin/stardust
```

The default listener is `127.0.0.1:8787`. Useful options:

```sh
./bin/stardust \
  --listen 127.0.0.1:8787 \
  --collection-timeout 10s \
  --request-timeout 5s \
  --disk-path /
```

The agent intentionally does not expose a public listening address unless an
operator explicitly changes `--listen`.

## API contract

All endpoints are under `/api/v1`. Successful responses include an
`api_version` field and an RFC 3339 UTC `timestamp`. Errors use the same
version field and the following shape:

```json
{
  "api_version": "v1",
  "error": {
    "code": "not_found",
    "message": "endpoint not found"
  }
}
```

### `GET /api/v1/health`

Returns HTTP 200 when the process is healthy:

```json
{
  "api_version": "v1",
  "status": "ok",
  "timestamp": "2026-01-01T12:00:00Z"
}
```

### `GET /api/v1/metrics`

Returns HTTP 200 with a `data` object. Values use the host's current state when
each metric is collected; fields are not an atomic, same-instant snapshot.
Metric semantics:

- `hostname` and `os` come from host OS metadata; `uptime_seconds` is elapsed
  seconds since boot.
- `cpu.usage_percent` is the system-wide, non-blocking CPU usage delta since
  the previous CPU sample, not an instantaneous reading or a fixed-interval
  average. The first sample can be unavailable until a baseline exists.
  `logical_cores` is the count of online logical CPUs.
- `memory` reports host virtual memory used/total bytes and used percentage as
  provided by the OS metrics library.
- `disk` reports usage for the filesystem containing the configured
  `--disk-path`; sizes are bytes and the percentage is 0-100.
- `network` reports cumulative bytes and packets sent/received, summed across
  all interfaces reported by the OS, including loopback and virtual interfaces.
  These counters can reset on reboot or interface reset, and forwarded traffic
  may be counted on more than one interface.

If an individual metric is unavailable, the response still returns available
data and includes a `data.errors` array with the metric name and reason. The
collector checks its deadline between metric reads; a collection timeout
returns HTTP 503.

```json
{
  "api_version": "v1",
  "timestamp": "2026-01-01T12:00:00Z",
  "data": {
    "hostname": "server-01",
    "os": {
      "name": "linux",
      "platform": "ubuntu",
      "platform_version": "24.04",
      "kernel_arch": "x86_64"
    },
    "cpu": {"usage_percent": 12.5, "logical_cores": 4},
    "memory": {"used_bytes": 1000, "total_bytes": 2000, "used_percent": 50},
    "disk": {"path": "/", "used_bytes": 1000, "total_bytes": 2000, "used_percent": 50},
    "uptime_seconds": 86400,
    "network": {
      "bytes_received": 1000,
      "bytes_sent": 500,
      "packets_received": 20,
      "packets_sent": 10
    }
  }
}
```

## systemd

Create a service account and install the binary and unit:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin stardust
sudo install -m 0755 bin/stardust /usr/local/bin/stardust
sudo install -m 0644 deploy/systemd/stardust.service /etc/systemd/system/stardust.service
sudo systemctl daemon-reload
sudo systemctl enable --now stardust
```

Forward the loopback port to the Orbit workstation:

```sh
ssh -N -L 8787:127.0.0.1:8787 user@server
```

## Development

```sh
make fmt
make test
make build
make lint
```

The release workflow uses the shared GoReleaser workflow and publishes a GitHub
release for every push to `main`. It automatically selects the next semantic
version from published releases; manual dispatch follows the same path.
