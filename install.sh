#!/usr/bin/env bash
set -euo pipefail

repo="ch55secake/stardust"
install_dir="${STARDUST_INSTALL_DIR:-}"
version=""
install_service=true

usage() {
	cat <<'EOF'
Install the latest Stardust release for Linux or macOS.

Usage: install.sh [--install-dir DIR] [--version vX.Y.Z] [--no-service]

Options:
  --install-dir DIR  Install Stardust into DIR (default: /usr/local/bin)
  --version VERSION  Install a specific release (default: latest)
  --no-service       Install only the binary; do not create a service
  -h, --help         Show this help

The install directory can also be set with STARDUST_INSTALL_DIR. Service installs
require root; the macOS LaunchDaemon runs as the invoking user.
EOF
}

fail() {
	printf 'stardust installer: %s\n' "$*" >&2
	exit 1
}

while (($#)); do
	case "$1" in
	--install-dir)
		(($# >= 2)) || fail "--install-dir requires a directory"
		install_dir="$2"
		shift 2
		;;
	--version)
		(($# >= 2)) || fail "--version requires a version like v1.2.3"
		version="$2"
		shift 2
		;;
	--no-service)
		install_service=false
		shift
		;;
	-h|--help)
		usage
		exit 0
		;;
	*)
		usage >&2
		fail "unknown option: $1"
		;;
	esac
done

if [[ -n "$version" && ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	fail "invalid version '$version' (expected vX.Y.Z)"
fi

case "$(uname -s)" in
Linux)
	goos=linux
	service_manager=systemd
	[[ -n "$install_dir" ]] || install_dir=/usr/local/bin
	;;
Darwin)
	goos=darwin
	service_manager=launchd
	[[ -n "$install_dir" ]] || install_dir=/usr/local/bin
	;;
*) fail "only Linux and macOS are supported" ;;
esac
[[ -n "$install_dir" ]] || fail "install directory must not be empty"

case "$(uname -m)" in
x86_64|amd64) arch=amd64 ;;
aarch64|arm64) arch=arm64 ;;
*) fail "unsupported architecture: $(uname -m) (supported: x86_64, aarch64)" ;;
esac

for tool in curl tar awk install mktemp; do
	command -v "$tool" >/dev/null 2>&1 || fail "required command not found: $tool"
done
case "$goos" in
	linux)
		command -v sha256sum >/dev/null 2>&1 || fail "required command not found: sha256sum"
		;;
	darwin)
		command -v shasum >/dev/null 2>&1 || fail "required command not found: shasum"
		;;
esac

if [[ "$install_service" == true ]]; then
	if [[ "$service_manager" == systemd ]]; then
		case "$install_dir" in
			/usr/local/bin|/usr/local/bin/) ;;
			*) fail "the systemd unit uses /usr/local/bin; use --no-service for a custom install directory" ;;
		esac
		(( EUID == 0 )) || fail "systemd installation requires root; use sudo or pass --no-service"
		for tool in systemctl systemd-analyze getent groupadd useradd; do
			command -v "$tool" >/dev/null 2>&1 || fail "required command not found: $tool (or pass --no-service)"
		done
		systemctl show-environment >/dev/null 2>&1 || fail "systemd is not running; pass --no-service to install only the binary"
	else
		(( EUID == 0 )) || fail "macOS LaunchDaemon installation requires sudo; rerun with sudo or pass --no-service"
		[[ -n "${SUDO_USER:-}" && "$SUDO_USER" != root ]] || fail "run sudo from the account that should own Stardust; direct root installation is unsupported"
		for tool in launchctl plutil id chown; do
			command -v "$tool" >/dev/null 2>&1 || fail "required command not found: $tool (or pass --no-service)"
		done
		launchd_domain=system
		launchd_label=com.ch55secake.stardust
		launchd_target="${launchd_domain}/${launchd_label}"
		launchd_user="$SUDO_USER"
		launchd_group="$(id -gn "$launchd_user")" || fail "could not find the primary group for ${launchd_user}"
		launchd_log_dir=/Library/Logs/Stardust
		launchctl print system >/dev/null 2>&1 || fail "launchd system domain is unavailable; pass --no-service to install only the binary"
	fi
fi

if [[ -n "$version" ]]; then
	release_url="https://github.com/${repo}/releases/download/${version}"
else
	release_url="https://github.com/${repo}/releases/latest/download"
fi

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/stardust-install.XXXXXX")"
staged_path=""
cleanup() {
	rm -rf "$tmp_dir"
	if [[ -n "$staged_path" ]]; then
		rm -f "$staged_path"
	fi
}
trap cleanup EXIT

checksums_path="${tmp_dir}/checksums.txt"
curl --fail --location --silent --show-error --proto '=https' --tlsv1.2 \
	--output "$checksums_path" "${release_url}/checksums.txt" || fail "could not download release checksums"

archive_name="$(awk -v arch="$arch" \
	-v goos="$goos" \
	'$2 ~ ("^stardust_.*_" goos "_" arch "\\.tar\\.gz$") { print $2; exit }' \
	"$checksums_path")"
[[ -n "$archive_name" ]] || fail "release has no ${goos} ${arch} archive"
archive_version="${archive_name#stardust_}"
archive_version="${archive_version%_${goos}_${arch}.tar.gz}"
[[ "$archive_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "invalid release archive name: ${archive_name}"
release_tag="v${archive_version}"

expected_checksum="$(awk -v asset="$archive_name" '$2 == asset { print $1; exit }' "$checksums_path")"
[[ "$expected_checksum" =~ ^[[:xdigit:]]{64}$ ]] || fail "no valid SHA-256 checksum for ${archive_name}"

archive_path="${tmp_dir}/${archive_name}"
curl --fail --location --silent --show-error --proto '=https' --tlsv1.2 \
	--output "$archive_path" "${release_url}/${archive_name}" || fail "could not download ${archive_name}"

if [[ "$goos" == linux ]]; then
	actual_checksum="$(sha256sum "$archive_path" | awk '{ print $1 }')"
else
	actual_checksum="$(shasum -a 256 "$archive_path" | awk '{ print $1 }')"
fi
[[ "$actual_checksum" == "$expected_checksum" ]] || fail "checksum verification failed for ${archive_name}"

tar -xzf "$archive_path" -C "$tmp_dir" stardust || fail "could not extract Stardust from archive"
[[ -f "${tmp_dir}/stardust" ]] || fail "release archive did not contain the Stardust binary"

if [[ "$install_service" == true ]]; then
	if [[ "$service_manager" == systemd ]]; then
		unit_path="${tmp_dir}/stardust.service"
		curl --fail --location --silent --show-error --proto '=https' --tlsv1.2 \
			--output "$unit_path" "https://raw.githubusercontent.com/${repo}/${release_tag}/deploy/systemd/stardust.service" || fail "could not download the systemd unit for ${release_tag}"
		if ! getent group stardust >/dev/null; then
			groupadd --system stardust || fail "could not create the stardust service group"
		fi
		if ! getent passwd stardust >/dev/null; then
			useradd --system --gid stardust --no-create-home --shell /usr/sbin/nologin stardust || fail "could not create the stardust service user"
		fi
	else
		plist_template="${tmp_dir}/com.ch55secake.stardust.template.plist"
		plist_path="${tmp_dir}/com.ch55secake.stardust.plist"
		curl --fail --location --silent --show-error --proto '=https' --tlsv1.2 \
			--output "$plist_template" "https://raw.githubusercontent.com/${repo}/${release_tag}/deploy/launchd/${launchd_label}.plist" || fail "could not download the LaunchDaemon plist for ${release_tag}"
		cp "$plist_template" "$plist_path"
		plutil -replace ProgramArguments.0 -string "${install_dir}/stardust" "$plist_path" || fail "could not configure the LaunchDaemon binary path"
		plutil -replace UserName -string "$launchd_user" "$plist_path" || fail "could not configure the LaunchDaemon user"
		plutil -replace GroupName -string "$launchd_group" "$plist_path" || fail "could not configure the LaunchDaemon group"
		plutil -replace StandardOutPath -string "${launchd_log_dir}/stdout.log" "$plist_path" || fail "could not configure the LaunchDaemon log path"
		plutil -replace StandardErrorPath -string "${launchd_log_dir}/stderr.log" "$plist_path" || fail "could not configure the LaunchDaemon error log path"
		plutil -lint "$plist_path" || fail "the LaunchDaemon plist failed validation"
	fi
fi

mkdir -p "$install_dir" || fail "cannot create ${install_dir}; choose a writable directory with --install-dir"
staged_path="$(mktemp "${install_dir}/.stardust.XXXXXX")" || fail "cannot write to ${install_dir}; choose a writable directory with --install-dir"
install -m 0755 "${tmp_dir}/stardust" "$staged_path" || fail "could not install the Stardust binary"
mv -f "$staged_path" "${install_dir}/stardust" || fail "could not move Stardust into ${install_dir}"
staged_path=""

printf 'Installed Stardust (%s) to %s/stardust\n' "$archive_name" "${install_dir%/}"

if [[ "$install_service" == true ]]; then
	if [[ "$service_manager" == systemd ]]; then
		systemd-analyze verify "$unit_path" || fail "the systemd unit failed verification"
		install -m 0644 "$unit_path" /etc/systemd/system/stardust.service || fail "could not install the systemd unit"
		systemctl daemon-reload || fail "could not reload systemd units"
		systemctl enable stardust.service || fail "could not enable the stardust service"
		if systemctl is-active --quiet stardust.service; then
			systemctl restart stardust.service || fail "could not restart stardust; inspect 'journalctl -u stardust'"
		else
			systemctl start stardust.service || fail "could not start stardust; inspect 'journalctl -u stardust'"
		fi
		printf 'Enabled and started systemd service stardust. Check it with: systemctl status stardust\n'
	else
		mkdir -p "$launchd_log_dir"
		chown "$launchd_user:$launchd_group" "$launchd_log_dir" || fail "could not assign the Stardust log directory to ${launchd_user}"
		chmod 0750 "$launchd_log_dir"
		launchctl bootout "$launchd_target" >/dev/null 2>&1 || true
		install -o root -g wheel -m 0644 "$plist_path" "/Library/LaunchDaemons/${launchd_label}.plist" || fail "could not install the LaunchDaemon plist"
		launchctl enable "$launchd_target" || fail "could not enable the Stardust LaunchDaemon"
		launchctl bootstrap "$launchd_domain" "/Library/LaunchDaemons/${launchd_label}.plist" || fail "could not start the Stardust LaunchDaemon"
		printf 'Enabled and started LaunchDaemon %s as %s. Logs are in %s\n' "$launchd_label" "$launchd_user" "$launchd_log_dir"
	fi
fi
