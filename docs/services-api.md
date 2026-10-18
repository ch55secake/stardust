# Read-only systemd service inventory and logs (API v1)

This is the producer contract for Orbit's service browser and log viewer
([Orbit #44](https://github.com/ch55secake/orbit/issues/44),
[Orbit #45](https://github.com/ch55secake/orbit/issues/45)). It adds read-only
service metadata and allowlisted journal queries; it does **not** add restarts,
arbitrary process control, or launchd support. It does not invoke SSH commands;
the existing SSH forward can still transport API traffic.

## Explicit opt-in and read authorization

Inventory and service logs are disabled by default. Start Stardust with
`--service-policy-file=/path/to/services.json` to enable either capability. The
JSON file contains:

```json
{
  "token": "REPLACE_WITH_A_RANDOM_SECRET_OF_AT_LEAST_32_CHARACTERS",
  "units": ["nginx.service", "worker@blue.service"],
  "logs": {
    "token": "A_DIFFERENT_RANDOM_SECRET_FOR_LOG_READS",
    "units": ["nginx.service"]
  }
}
```

Generate a different cryptographically random token for inventory and logs (for
example, 32 random bytes encoded as hexadecimal). Never use the example values.
Each token must be 32–256 printable non-space ASCII characters. The file must be
regular (not a symlink), owned by the daemon user (or root if the daemon runs as
root), readable by the daemon, and mode **0400 or 0600**. Keep its parent
directory protected from untrusted writes. Never put either token in process
arguments, URLs, source control, or logs. The installer does not create this file
or change permissions/groups; administrator setup is explicit.

Each allowlist contains at most 128 exact `.service` names, including template
instances and aliases. No globs, paths, options, or shell expressions are accepted.
Duplicates collapse and names sort lexicographically. An empty list grants no
visibility. Only those names are queried, including inactive/failed units; this
is **not** an enumeration of every unit on the machine. Select instances such as
`worker@blue.service` rather than an uninstantiated template.

All inventory list/detail requests require `Authorization: Bearer <token>`.
Missing/incorrect credentials return 401 with a Bearer challenge; an authenticated
request for a non-allowlisted unit returns 403 without querying systemd. Token
hashes are compared in constant time. The inventory token grants read-only
service metadata access, not log-read or mutation permissions. Log access has an
independent token and exact `.service` allowlist under `logs`; reusing the
inventory token is rejected. An omitted `logs` object disables log access. Empty
log allowlists grant no access. Keep both secrets in the same private policy file
and deliver them to authorized clients separately. Clients must store each token
in a private credential file/store, add headers in memory (not command arguments),
and redact them from diagnostics. Never forward either token to another server.

This preserves the existing health/metrics authentication policy; it does **not**
resolve the broader same-host metrics protection issue
([#31](https://github.com/ch55secake/stardust/issues/31)). Numeric-loopback-only
binding remains enforced. HTTP is not encrypted: remote clients must use a
trusted SSH tunnel. Do not publish the port through an unauthenticated proxy.
Configuration and tokens load once at startup; rotate by replacing the protected
file and restarting the agent, then updating authorized clients. Restarting the
agent for administrator configuration is not an API service-restart capability.

## Capability discovery

`GET /api/v1/capabilities` requires no credential and returns no unit metadata:

```json
{
  "api_version": "v1",
  "timestamp": "2026-01-01T12:00:00Z",
  "services": {
    "enabled": true,
    "supported": true,
    "authentication": "bearer",
    "logs": true,
    "restart": false
  }
}
```

`enabled` means a service policy and inspector are configured. `supported` means
Linux with `/run/systemd/system` present; it is false on macOS and non-systemd
hosts. It is not a guarantee that D-Bus or systemctl is accessible. A supported
host with a missing binary or unavailable bus returns `services_unavailable`; an
access denial returns `services_permission_denied`. Older agents return 404 for capability discovery;
clients must keep their existing metrics view and mark inventory unsupported.
`logs` is true only when both the authorized log policy and supported journal
backend are configured.

## List and details

`GET /api/v1/services?limit=20&filter=worker&cursor=previous.service`

- `limit`: 1–50, default 20.
- `filter`: optional, case-sensitive unit-name substring, at most 255 bytes.
- `cursor`: optional last returned allowlisted name, exclusive. Treat this value
  as opaque, echoing the returned `next_cursor` and retaining the same filter.
- Duplicate/unknown/malformed query parameters return 400. The cursor must exist
  in the current configured allowlist. It is not a snapshot or authentication token.
- Entries follow sorted allowlist order. `next_cursor` is omitted on the final
  page. Empty results use `"units": []`. Each page reads live state; states/PIDs
  can change between or within requests. Restarting with a different policy may
  invalidate cursors; start pagination again in that case.

```json
{
  "api_version": "v1",
  "timestamp": "2026-01-01T12:00:00Z",
  "data": {
    "units": [{
      "name": "worker@blue.service",
      "id": "worker@blue.service",
      "description": "Background worker",
      "load_state": "loaded",
      "active_state": "active",
      "sub_state": "running",
      "unit_file_state": "enabled",
      "main_pid": 123,
      "main_process": {
        "name": "worker",
        "effective_uid": 1001
      }
    }]
  }
}
```

`GET /api/v1/services/{name}` returns the same unit object in `data`, not an array.
URL-encode unit names. `name` is the configured identity used for authorization;
`id` is systemd's canonical identity and may differ for an allowed alias. Allowing
an alias authorizes metadata for its current target; administrators must control
unit/alias definitions. No alias expansion grants additional API-visible names.

`main_pid: 0` means there is no main process; it is a live PID, not a stable process
handle. When `/proc` exposes the current process, `main_process` includes its
kernel process name and effective numeric UID; it may be omitted if the process
exits or changes during collection. No process command lines, environment,
executable paths, or credentials are returned. `unit_file_state` is systemd's
value (including static/disabled/etc.), or empty when unavailable. A missing
configured unit remains visible in lists with `load_state: "not-found"`,
`active_state: "inactive"`, `sub_state: "dead"`,
zero PID, and empty optional metadata. Direct detail lookup returns 404 instead.
Other backend errors fail the page, never masquerading as an empty inventory.

All service/capability responses use `Cache-Control: no-store`. Non-GET methods
return 405 and `Allow: GET`. Errors use the existing v1 error envelope:

| HTTP | Error code | Meaning |
| --- | --- | --- |
| 400 | `invalid_unit` / `invalid_query` | Invalid identity, query, or cursor |
| 401 | `unauthorized` | Missing/invalid service read credential |
| 403 | `unit_forbidden` | Unit outside configured policy |
| 404 | `unit_not_found` | Allowed unit does not exist |
| 501 | `services_unsupported` | Host does not support systemd inventory |
| 503 | `services_disabled` | Policy not configured |
| 503 | `services_permission_denied` | systemd access denied |
| 503 | `services_unavailable` | Backend missing or unavailable |
| 504 | `services_timeout` | Request deadline/cancellation |

## Read-only service logs

Log access uses a separate credential and allowlist; permission to browse service
metadata does not imply permission to read logs. The endpoint accepts no
filesystem path, journal namespace, arbitrary match expression, or command:

```text
GET /api/v1/services/{name}/logs?limit=50&since=2026-10-18T12:00:00Z&until=2026-10-18T13:00:00Z
Authorization: Bearer <distinct log-read token>
```

- `limit`: 1–100, default 50. Response body is capped at 2 MiB; individual
  `message` values are capped at 64 KiB and marked `message_truncated: true` if
  shortened on a UTF-8 boundary.
- `since` and `until`: optional RFC3339 timestamps interpreted by journalctl.
  `since` must not be later than `until`.
- `before`: opaque `next_cursor` for older history. Cursors are exclusive and
  responses remain oldest-to-newest. Keep the same time filters while paging.
- `after`: opaque `latest_cursor` for cursor-based follow polling. Entries after
  it are returned oldest-to-newest. Poll again with the last `latest_cursor`
  when `has_more` is true; if no entries arrive, retain the supplied cursor.
  Polling avoids a long-lived stream and naturally supports request cancellation.
- `before` and `after` cannot be combined. Duplicate/unknown parameters, invalid
  timestamps, malformed cursor encodings, and cross-unit cursors return 400.
  Expired, rotated, tampered, or restart-invalidated cursors return 410
  `log_cursor_expired`; restart from recent history.
- Empty results use `"entries": []`, not null. Journal vacuuming between requests
  can expire cursors. Boot changes do not invalidate cursors while their entry is
  available. Time-window filters can restart history after expiration, but
  overlapping windows may contain duplicates.

```json
{
  "api_version": "v1",
  "timestamp": "2026-10-18T12:30:00Z",
  "data": {
    "unit": "worker.service",
    "entries": [{
      "timestamp": "2026-10-18T12:29:59.123456Z",
      "priority": 6,
      "message": "worker started\nready ✓",
      "cursor": "opaque-unit-bound-cursor"
    }],
    "next_cursor": "opaque-cursor-for-older-history",
    "latest_cursor": "opaque-cursor-for-follow-polling",
    "has_more": true
  }
}
```

`unit` is systemd's canonical unit identity (which may differ from an allowlisted
alias in the request path). The cursor is an encrypted, unit-bound, process-local
token for systemd's private journal cursor, not an authorization token. A
Stardust restart invalidates issued
cursors; clients should recover from `log_cursor_expired` by requesting recent
history. `timestamp` is UTC with microsecond precision; `priority` is optional
syslog priority 0–7. `message` is untrusted log data: Stardust JSON-encodes
but does not evaluate, strip, or interpret terminal controls. Clients must render
it as text, preserving Unicode and multiline content while escaping terminal
controls. Journal messages may contain application secrets; Orbit must not
persist, export, or copy them without an explicit user action. Stardust never
logs returned messages, cursors, or bearer credentials.

`GET /api/v1/capabilities` sets `services.logs` true only when a log policy and
supported systemd-journal backend are configured; it remains false by default.
Logs are GET-only and use `Cache-Control: no-store`.

| HTTP | Error code | Meaning |
| --- | --- | --- |
| 400 | `invalid_unit` / `invalid_log_query` | Invalid unit, query, timestamp, or cursor |
| 401 | `logs_unauthorized` | Missing/invalid distinct log-read credential |
| 403 | `log_unit_forbidden` | Unit outside the log-specific allowlist |
| 410 | `log_cursor_expired` | Cursor expired, rotated, invalidated, or unusable |
| 501 | `logs_unsupported` | systemd journal is unsupported on this host |
| 503 | `logs_disabled` | Log policy is not configured |
| 503 | `logs_permission_denied` | Stardust lacks journal read permission |
| 503 | `logs_unavailable` | Backend failure or output exceeded bounds |
| 504 | `logs_timeout` | Request deadline/cancellation |

Stardust remains unprivileged and starts with no additional groups. The installer
does **not** add the account to `systemd-journal`: that group commonly grants
access to logs for every system service, beyond this API's per-unit allowlist. An
administrator may explicitly choose that broad host permission after reviewing
the threat model, or provision a separately managed narrowly scoped journal
broker. Without journal permission, requests fail closed with
`logs_permission_denied`; do not grant root, unrestricted sudo, or arbitrary file
read access to enable this endpoint. There is no `journalctl`-over-SSH fallback.
Only fixed-argument `/usr/bin/journalctl` JSON reads are executed with a canonical
unit ID resolved from an allowlisted unit, without a shell, pager, or inherited
environment. At most four log requests inspect/read concurrently; excess
requests wait under the configured request deadline. Each request has bounded
output. Stardust does not cache or retain logs.

## Execution and validation limits

Stardust stays unprivileged. Reads use `/usr/bin/systemctl show` with a fixed
property list and argument vector, no shell or PATH lookup, and a minimal fixed
environment. Deployments with systemctl elsewhere are currently unavailable.
Output is capped at 64 KiB per unit; the whole page shares the HTTP request
deadline, and process pipe cleanup is bounded. No sudo, PolicyKit changes,
supplementary groups, or service mutations are introduced. Service descriptions
remain untrusted data; Orbit must neutralize terminal control sequences before
rendering them. Kernel process names are sanitized for control characters but
should still be rendered as text.

Automated tests use fake systemd output and API fixtures; they do not inspect or
modify real services. A separately authorized disposable Linux host may be used
to check read behavior as the actual unprivileged Stardust user (active/inactive/
failed unit, alias, missing unit, bus permission denial, and unsupported host).
Do not alter production units just to create test cases.
