# Read-only systemd service inventory (API v1)

This is the producer contract for Orbit's service browser
([Orbit #44](https://github.com/ch55secake/orbit/issues/44)). It does **not** add
logs, restarts, arbitrary process control, or launchd support. It does not invoke
SSH commands; the existing SSH forward can still transport API traffic.

## Explicit opt-in and read authorization

Inventory is disabled by default. Start Stardust with
`--service-policy-file=/path/to/services.json` to enable it. The JSON file contains:

```json
{
  "token": "REPLACE_WITH_A_RANDOM_SECRET_OF_AT_LEAST_32_CHARACTERS",
  "units": ["nginx.service", "worker@blue.service"]
}
```

Generate a cryptographically random token (for example, 32 random bytes encoded
as hexadecimal). Never use the example value. The token must be 32–256 printable
non-space ASCII characters. The file must be regular (not a symlink), owned by
root or the daemon user, readable by the daemon, and mode **0400 or 0600**. Keep
its parent directory protected from untrusted writes. Never put the token in
process arguments, URLs, source control, or logs. The installer does not create
this file or change permissions/groups; administrator setup is explicit.

The allowlist contains at most 128 exact `.service` names, including template
instances and aliases. No globs, paths, options, or shell expressions are accepted.
Duplicates collapse and names sort lexicographically. An empty list grants no
visibility. Only those names are queried, including inactive/failed units; this
is **not** an enumeration of every unit on the machine. Select instances such as
`worker@blue.service` rather than an uninstantiated template.

All inventory list/detail requests require `Authorization: Bearer <token>`.
Missing/incorrect credentials return 401 with a Bearer challenge; an authenticated
request for a non-allowlisted unit returns 403 without querying systemd. Token
hashes are compared in constant time. This token grants read-only service access,
not log-read or mutation permissions. Treat descriptions/unit identities as host
metadata that may be sensitive. Client applications must store the token in a
private credential file/store, add the header in memory (not command arguments),
and redact it from diagnostics. Never forward it to a different server.

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
  "services": {
    "enabled": true,
    "supported": true,
    "authentication": "bearer",
    "logs": false,
    "restart": false
  }
}
```

`enabled` means a service policy and inspector are configured. `supported` means
Linux with `/run/systemd/system` present; it is false on macOS and non-systemd
hosts. It is not a guarantee that D-Bus or systemctl is accessible. A supported
host with a missing binary, unavailable bus, or insufficient permissions returns
503 on inventory requests. Older agents return 404 for capability discovery;
clients must keep their existing metrics view and mark inventory unsupported.

## List and details

`GET /api/v1/services?limit=20&filter=worker&cursor=previous.service`

- `limit`: 1–50, default 20.
- `filter`: optional, case-sensitive unit-name substring, at most 255 bytes.
- `cursor`: optional last examined allowlisted name, exclusive. Treat this value
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
  "data": {
    "units": [{
      "name": "worker@blue.service",
      "id": "worker@blue.service",
      "description": "Background worker",
      "load_state": "loaded",
      "active_state": "active",
      "sub_state": "running",
      "unit_file_state": "enabled",
      "main_pid": 123
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
handle. No process command lines, environment, executable paths, or credentials
are returned. `unit_file_state` is systemd's value (including static/disabled/etc.),
or empty when unavailable. A missing configured unit remains visible in lists
with `load_state: "not-found"`, `active_state: "inactive"`, `sub_state: "dead"`,
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
| 503 | `services_unavailable` | Backend missing/unavailable or permission denied |
| 504 | `services_timeout` | Request deadline/cancellation |

## Execution and validation limits

Stardust stays unprivileged. Reads use `/usr/bin/systemctl show` with a fixed
property list and argument vector, no shell or PATH lookup, and a minimal fixed
environment. Deployments with systemctl elsewhere are currently unavailable.
Output is capped at 64 KiB per unit; the whole page shares the HTTP request
deadline, and process pipe cleanup is bounded. No sudo, PolicyKit changes,
supplementary groups, or service mutations are introduced. Service descriptions
remain untrusted data; Orbit must neutralize terminal control sequences before
rendering them.

Automated tests use fake systemd output and API fixtures; they do not inspect or
modify real services. A separately authorized disposable Linux host may be used
to check read behavior as the actual unprivileged Stardust user (active/inactive/
failed unit, alias, missing unit, bus permission denial, and unsupported host).
Do not alter production units just to create test cases.
