# Stardust

Stardust is a lightweight Linux and macOS telemetry agent for Orbit. It runs
without root privileges, keeps no persistent state, and binds to loopback by
default so Orbit can reach it through an SSH port forward.

## Features

- CPU, memory, disk, uptime, network, hostname, and operating system metrics
- Small versioned JSON API using only the Go standard HTTP server
- Configurable collection and request timeouts
- Graceful shutdown and useful lifecycle logging
- Hardened systemd unit in `deploy/systemd/stardust.service`
- macOS LaunchDaemon in `deploy/launchd/com.ch55secake.stardust.plist`

## Install

Install the latest release with one command. On Linux, use:

```sh
curl -fsSL https://raw.githubusercontent.com/ch55secake/stardust/main/install.sh | sudo bash
```

On macOS, use `sudo` so the installer can install and start a system LaunchDaemon:

```sh
curl -fsSL https://raw.githubusercontent.com/ch55secake/stardust/main/install.sh | sudo bash
```

The installer detects `amd64` or `arm64` and verifies the archive against the
published SHA-256 checksum. On Linux, it installs the binary, creates the
unprivileged `stardust` account, and enables and starts the hardened systemd
service. On macOS, it installs the binary under `/usr/local/bin` and starts a
LaunchDaemon as the account that invoked `sudo`. This works without an active
GUI login session. To install only the binary into a custom directory on either
platform, use:

```sh
curl -fsSL https://raw.githubusercontent.com/ch55secake/stardust/main/install.sh | bash -s -- --no-service --install-dir "$HOME/.local/bin"
```

To install a specific release, add `--version vX.Y.Z` to either command.

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

Listener ports must be decimal TCP ports from `1` through `65535`; port `0` is
rejected because Stardust does not report an OS-assigned ephemeral port.

Stardust has no API authentication or TLS, so it only accepts numeric loopback
listeners (`127.0.0.1` or `[::1]`). Wildcard addresses, hostnames (including
`localhost`), and non-loopback IP addresses are rejected at startup with an
actionable configuration error. Remote access is supported through SSH port
forwarding; direct non-loopback deployment is not supported.

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
- `swap` reports swap total, used, and free bytes and used percentage. A zero
  total means the host has no configured swap; an unavailable metric is listed
  in `data.errors` without failing the rest of the snapshot.
- `disk` reports usage for the filesystem containing the configured
  `--disk-path`; sizes are bytes and the percentage is 0-100.
- `network` reports cumulative bytes and packets sent/received, summed across
  all interfaces reported by the OS, including loopback and virtual interfaces.
  Its `interfaces` array reports the same counters by interface name. These
  counters can reset on reboot or interface reset, and forwarded traffic may be
  counted on more than one interface. They are cumulative totals, not rates.

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
    "swap": {"total_bytes": 1000, "used_bytes": 300, "free_bytes": 700, "used_percent": 30},
    "disk": {"path": "/", "used_bytes": 1000, "total_bytes": 2000, "used_percent": 50},
    "uptime_seconds": 86400,
    "network": {
      "bytes_received": 1000,
      "bytes_sent": 500,
      "packets_received": 20,
      "packets_sent": 10,
      "interfaces": [
        {
          "name": "eth0",
          "bytes_received": 1000,
          "bytes_sent": 500,
          "packets_received": 20,
          "packets_sent": 10
        }
      ]
    }
  }
}
```

## systemd

On Linux, the installer does this setup automatically. To install manually
(for example, from a source build), create a service account and install the
binary and unit:

```sh
sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin stardust
sudo install -m 0755 bin/stardust /usr/local/bin/stardust
sudo install -m 0644 deploy/systemd/stardust.service /etc/systemd/system/stardust.service
sudo systemctl daemon-reload
sudo systemctl enable --now stardust
```

The unit creates `/run/stardust` as a private runtime directory owned by the
service account and removes it when the service stops. On a disposable Linux
machine with systemd, the deployment can be exercised with `make build` followed
by `sudo bash deploy/systemd/test.sh`; the test installs, starts, restarts, and
stops the service, then removes its unit file.

Forward the loopback port to the Orbit workstation:

```sh
ssh -N -L 8787:127.0.0.1:8787 user@server
```

## macOS LaunchDaemon

The macOS installer creates `/Library/LaunchDaemons/com.ch55secake.stardust.plist`
and runs Stardust as the user who invoked `sudo` (not as root). The service
starts at boot and launchd restarts it if it exits unsuccessfully. Logs are
written to `/Library/Logs/Stardust/`.

Inspect or stop the service with:

```sh
sudo launchctl print system/com.ch55secake.stardust
sudo launchctl bootout system/com.ch55secake.stardust
```

To change the listener port, update the third argument in the plist and reload
the service (this example uses port `28787`):

```sh
sudo /usr/libexec/PlistBuddy \
  -c 'Set :ProgramArguments:2 127.0.0.1:28787' \
  /Library/LaunchDaemons/com.ch55secake.stardust.plist
sudo launchctl bootout system/com.ch55secake.stardust
sudo launchctl bootstrap system /Library/LaunchDaemons/com.ch55secake.stardust.plist
```

For binary-only installation, use `--no-service` and manage the process with
your own supervisor.

## Development

```sh
make fmt
make test
make build
make lint
```

The release workflow uses the shared GoReleaser workflow and publishes a GitHub
release for every push to `main`. It automatically selects the next semantic
version from published releases; manual dispatch follows the same path. Release
archives are built for Linux and macOS on amd64 and arm64.
