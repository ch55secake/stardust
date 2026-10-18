#!/usr/bin/env bash
set -euo pipefail

fail() {
	printf 'systemd integration test: %s\n' "$*" >&2
	exit 1
}

if (( EUID != 0 )); then
	fail "run this test as root on a disposable Linux machine with systemd"
fi

for tool in systemctl systemd-analyze journalctl curl ss useradd groupadd getent ps stat; do
	command -v "$tool" >/dev/null 2>&1 || fail "required command not found: $tool"
done

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
binary="${repo_root}/bin/stardust"
unit_source="${repo_root}/deploy/systemd/stardust.service"
unit_destination="/etc/systemd/system/stardust.service"
installed_binary="/usr/local/bin/stardust"

[[ -x "$binary" ]] || fail "build the binary first with 'make build'"
[[ -f "$unit_source" ]] || fail "service unit not found: ${unit_source}"
systemctl show-environment >/dev/null 2>&1 || fail "systemd is not running as the system service manager"
if systemctl cat stardust.service >/dev/null 2>&1; then
	fail "stardust.service already exists; use a disposable machine"
fi

if ! getent group stardust >/dev/null; then
	groupadd --system stardust
fi
if ! getent passwd stardust >/dev/null; then
	useradd --system --gid stardust --no-create-home --shell /usr/sbin/nologin stardust
fi

cleanup() {
	systemctl disable --now stardust.service >/dev/null 2>&1 || true
	rm -f "$unit_destination"
	systemctl daemon-reload >/dev/null 2>&1 || true
}
trap cleanup EXIT

install -m 0755 "$binary" "$installed_binary"
install -m 0644 "$unit_source" "$unit_destination"
systemd-analyze verify "$unit_destination"
systemctl daemon-reload
systemctl enable --now stardust.service

wait_for_health() {
	for _ in {1..30}; do
		if curl --fail --silent http://127.0.0.1:8787/api/v1/health >/dev/null; then
			return 0
		fi
		sleep 1
	done
	return 1
}

wait_for_health || fail "health endpoint did not become ready"
curl --fail --silent --max-time 10 http://127.0.0.1:8787/api/v1/metrics | grep -q '"data"' || fail "metrics endpoint did not return data"

[[ "$(stat -c '%U:%G' /run/stardust)" == "stardust:stardust" ]] || fail "runtime directory is not owned by stardust"
[[ "$(stat -c '%a' /run/stardust)" == 750 ]] || fail "runtime directory mode is not 0750"
[[ "$(stat -c '%U:%G' /var/lib/stardust)" == "stardust:stardust" ]] || fail "state directory is not owned by stardust"
[[ "$(stat -c '%a' /var/lib/stardust)" == 700 ]] || fail "state directory mode is not 0700"

main_pid="$(systemctl show --property=MainPID --value stardust.service)"
[[ "$main_pid" =~ ^[1-9][0-9]*$ ]] || fail "systemd reported invalid main PID: ${main_pid}"
process_user="$(ps -o user= -p "$main_pid" | xargs)"
[[ "$process_user" == stardust ]] || fail "service runs as ${process_user}, expected stardust"
[[ "$(systemctl show --property=User --value stardust.service)" == stardust ]] || fail "unit does not configure User=stardust"

listener_addresses="$(ss -ltnH '( sport = :8787 )' | awk '{ print $4 }')"
[[ "$listener_addresses" == "127.0.0.1:8787" ]] || fail "listener addresses are not loopback-only: ${listener_addresses}"

previous_pid="$main_pid"
systemctl kill --signal=SIGKILL stardust.service
for _ in {1..30}; do
	main_pid="$(systemctl show --property=MainPID --value stardust.service)"
	if [[ "$main_pid" =~ ^[1-9][0-9]*$ && "$main_pid" != "$previous_pid" ]] && systemctl is-active --quiet stardust.service; then
		break
	fi
	sleep 1
done
[[ "$main_pid" =~ ^[1-9][0-9]*$ && "$main_pid" != "$previous_pid" ]] || fail "systemd did not restart the failed service"
wait_for_health || fail "health endpoint did not recover after restart"

systemctl stop stardust.service
[[ ! -e /run/stardust ]] || fail "systemd did not remove the runtime directory after stop"
logs="$(journalctl --unit=stardust.service --no-pager)"
grep -q 'stardust listening' <<< "$logs" || fail "service startup was not logged"
grep -q 'stardust shutting down' <<< "$logs" || fail "graceful shutdown was not logged"

printf 'systemd integration test passed: validated unit, non-root service, loopback API, restart, and logs\n'
