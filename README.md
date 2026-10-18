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
LaunchDaemon as the dedicated `_stardust` account, not the invoking user. This
works without an active GUI login session. To install only the binary into a
custom directory on either platform, use:

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

Stardust only accepts numeric loopback listeners (`127.0.0.1` or `[::1]`); it
does not support TLS or direct non-loopback deployment. Remote access is
supported through SSH port forwarding. Loopback prevents remote network access,
but does not prevent other local accounts from connecting. For backward
compatibility, health and metrics remain unauthenticated by default; enable the
optional bearer token for multi-user hosts. This protects against other local
accounts, but not root or processes running as Stardust's own account. The
optional service inventory uses a separate bearer token when enabled.

To enable authentication, provision a cryptographically random token (for
example, 32 random bytes encoded as hexadecimal) in a regular file owned by the
runtime account with mode `0400` or `0600`, then start Stardust with
`--api-token-file /path/to/private-token`. The file must not be a symlink and
must be under root- or runtime-owned directories not writable by group or other
users (root-owned sticky directories such as `/tmp` are allowed). It must contain
at least 32 printable non-space ASCII characters (a trailing newline is allowed).
Stardust reads it once at startup and requires `Authorization: Bearer <token>` on
**both** health and metrics endpoints. Do not pass the token as a command-line
option or write it to logs; only the file path is passed as configuration. Store
and rotate the token using a private credential store, and configure clients to
add it to request headers without logging it. To rotate it, replace the file
securely and restart Stardust, then update client credentials. Authentication
does not enable non-loopback listeners or provide TLS; use the existing SSH
tunnel for remote access.

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
  `logical_cores` is the count of online logical CPUs. `per_core_usage_percent`
  contains a non-blocking usage percentage for each logical CPU, in the order
  reported by the OS metrics library; each value ranges from 0 to 100. These
  values are deltas since the previous per-core sample. `load_average` contains
  the 1-, 5-, and 15-minute scheduling load averages where supported (Linux and
  macOS); it is omitted and reported in `data.errors` when unavailable. Load
  averages are not percentages and can exceed the logical CPU count.
- `memory` reports host virtual memory used/total bytes and used percentage as
  provided by the OS metrics library.
- `swap` reports swap total, used, and free bytes and used percentage. A zero
  total means the host has no configured swap; an unavailable metric is listed
  in `data.errors` without failing the rest of the snapshot.
- `disk` reports usage for the filesystem containing the configured
  `--disk-path`; sizes are bytes and the percentage is 0-100.
- `filesystems` reports up to 64 mounted filesystems, sorted by mount point.
  Entries include the mount point, device and filesystem type where available,
  used/total bytes, unprivileged available bytes, and used percentage. Known
  kernel/pseudo filesystems (such as procfs, sysfs, and devfs) and empty mount
  points are skipped; memory-backed filesystems such as tmpfs are included.
  Normalized duplicate mount points collapse to one entry, choosing the
  non-empty device with the lexicographically first identity. Excess mounts are
  omitted with a `filesystems` error. A mount that cannot be read is omitted and
  reported in `data.errors` without discarding other filesystem or system data.
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
    "cpu": {
      "usage_percent": 12.5,
      "logical_cores": 4,
      "per_core_usage_percent": [10, 15, 20, 5],
      "load_average": {
        "one_minute": 0.5,
        "five_minutes": 0.75,
        "fifteen_minutes": 1.25
      }
    },
    "memory": {"used_bytes": 1000, "total_bytes": 2000, "used_percent": 50},
    "swap": {"total_bytes": 1000, "used_bytes": 300, "free_bytes": 700, "used_percent": 30},
    "disk": {"path": "/", "used_bytes": 1000, "total_bytes": 2000, "used_percent": 50},
    "filesystems": [
      {
        "mount_point": "/",
        "device": "/dev/root",
        "filesystem_type": "ext4",
        "used_bytes": 1000,
        "total_bytes": 2000,
        "available_bytes": 900,
        "used_percent": 50
      }
    ],
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

### Read-only systemd services and logs

Optional authenticated service list/detail and separately authorized service log
endpoints are documented in
[`docs/services-api.md`](docs/services-api.md), including capability discovery,
pagination, errors, and client credential handling. Inventory and logs are
disabled by default; inventory requires a private policy file with a read token
and exact unit allowlist. Logs require a distinct log-read token and independent
unit allowlist. Journal permission is never added by the installer. This does not
grant restart privileges, change the existing health/metrics policy, or add
support for non-loopback listeners.

### Installing the agent service

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

The installer validates any existing `stardust` account before reusing it. It
must be non-root, have no login shell or usable home directory, use the
`stardust` primary group, and have no other group memberships. Resolve an unsafe
existing account manually; the installer will not modify or delete it.

Forward the loopback port to the Orbit workstation:

```sh
ssh -N -L 8787:127.0.0.1:8787 user@server
```

## macOS LaunchDaemon

Installing the macOS LaunchDaemon requires `sudo`, but the service does not run
as root or as the invoking administrator. The installer creates (or safely
reuses) a dedicated hidden `_stardust` account and private `_stardust` group,
with a disabled login, no personal home directory, and no supplementary group
memberships configured on the account. It assigns unused system-range IDs and
fails rather than changing an account or ID that conflicts with existing users
or groups.

The executable and `/Library/LaunchDaemons/com.ch55secake.stardust.plist` are
installed as root-owned files. Stardust's log directory is owned by `_stardust`
and restricted to that account and group. Upgrades reuse the account without
removing it or its data; `--no-service` installs only the binary and does not
create or modify the service account. Resolve unsafe pre-existing `_stardust`
accounts manually. The service starts at boot and launchd restarts it if it exits
unsuccessfully. Logs are written to `/Library/Logs/Stardust/`. Removing the
LaunchDaemon or binary does not remove the account or logs; inspect and remove
those separately only when they are no longer needed.

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
