#!/usr/bin/env bash
set -euo pipefail

repo="ch55secake/stardust"
install_dir="${STARDUST_INSTALL_DIR:-}"
version=""
install_service=true
enable_service_inventory=false
service_policy_dir=/etc/stardust
service_policy_path="${service_policy_dir}/services.json"
default_service_units=(
	sshd.service ssh.service
	cron.service crond.service
	systemd-resolved.service systemd-networkd.service NetworkManager.service
	nginx.service apache2.service httpd.service
	docker.service containerd.service
	mariadb.service mysql.service postgresql.service
)
trusted_release_public_keys=(
'-----BEGIN PUBLIC KEY-----
MIIBojANBgkqhkiG9w0BAQEFAAOCAY8AMIIBigKCAYEAsE8IMKMvZ2rFpW9n/PYJ
Sg/QveK7C0MhpOLb9WjnO9NtqWIqaNh/iIwZaYUUpBhbf5wrqiFZTQN2K73+i4NQ
0b254yUPjsz7FkZY2Ynh9IFZlPl3ZESi668/8ER8a/EgDiQRaxmqMillXmL5Hdmq
qgptWxfFcCzeThZTsvevoDnnSTcUQJL4qWPAwE1lOLlnaL0OGYX6nyrjtPORJDKF
NZ6p+WWoAXC3BPUqt6LMSFzekgf1+HxYG24nffxAfk8QeK85OJd2QLFSRTvlR0iG
OBANmkGBfQ5oQ28C5WugjD3xajnx3HzE/+ENp3PNtlDjpeRwbFDRxbXT+O2YQk9e
avtH93zk5/SF+7FLyuTaI22vXmJsVsC/2mwLpC8qT4G0WxCBp3s9N4zT6ByJ/Vp1
6bnMnyf52Dt6tsfvJMb+lCx7yHeJUyYuKnDBWQ1anWktNMlQHut3+x6ObtVwA0Cf
/VXzJnAUmY/bheANHj0JYC3kW4EzHMnaXWAYgFcMzPc1AgMBAAE=
-----END PUBLIC KEY-----'
)

usage() {
	cat <<'EOF'
Install the latest Stardust release for Linux or macOS.

Usage: install.sh [--install-dir DIR] [--version vX.Y.Z] [--no-service] [--enable-service-inventory]

Options:
  --install-dir DIR  Install Stardust into DIR (default: /usr/local/bin)
  --version VERSION  Install a specific release (default: latest)
  --no-service       Install only the binary; do not create a service
  --enable-service-inventory  Opt in to an authenticated allowlist of installed common Linux services
  -h, --help         Show this help

The install directory can also be set with STARDUST_INSTALL_DIR. Service installs
require root; the macOS LaunchDaemon runs as the dedicated _stardust account.
EOF
}

fail() {
	printf 'stardust installer: %s\n' "$*" >&2
	exit 1
}

ensure_linux_service_account() {
	local group_entry group_name group_password group_gid group_members
	local passwd_entry user_name user_password user_uid user_gid user_gecos user_home user_shell
	local supplementary_gids supplementary_gid

	if ! getent group stardust >/dev/null; then
		groupadd --system stardust || fail "could not create the stardust service group"
	fi
	group_entry="$(getent group stardust)" || fail "could not inspect the stardust service group"
	IFS=: read -r group_name group_password group_gid group_members <<<"$group_entry"
	if [[ "$group_name" != stardust || ! "$group_gid" =~ ^[0-9]+$ ]] || ((10#$group_gid == 0)); then
		fail "existing stardust group is unsafe; it must have a nonzero numeric group ID"
	fi
	[[ -z "$group_members" || "$group_members" == stardust ]] || fail "existing stardust group is unsafe; it must not include other users"

	if ! getent passwd stardust >/dev/null; then
		useradd --system --gid stardust --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin stardust || fail "could not create the stardust service user"
	fi
	passwd_entry="$(getent passwd stardust)" || fail "could not inspect the stardust service account"
	IFS=: read -r user_name user_password user_uid user_gid user_gecos user_home user_shell <<<"$passwd_entry"
	if [[ "$user_name" != stardust || ! "$user_uid" =~ ^[0-9]+$ ]] || ((10#$user_uid == 0)); then
		fail "existing stardust account is unsafe; its user ID must be numeric and nonzero"
	fi
	if [[ ! "$user_gid" =~ ^[0-9]+$ || "$user_gid" != "$group_gid" ]]; then
		fail "existing stardust account is unsafe; its primary group must be stardust"
	fi
	case "$user_shell" in
		/usr/sbin/nologin|/sbin/nologin|/bin/false|/usr/bin/false) ;;
		*) fail "existing stardust account is unsafe; it must have a non-login shell" ;;
	esac
	case "$user_home" in
		""|/nonexistent|/dev/null) ;;
		*) [[ ! -d "$user_home" ]] || fail "existing stardust account is unsafe; its home directory must not exist" ;;
	esac

	supplementary_gids="$(id -G stardust)" || fail "could not inspect stardust supplementary groups"
	[[ -n "$supplementary_gids" ]] || fail "could not inspect stardust supplementary groups"
	for supplementary_gid in $supplementary_gids; do
		if [[ ! "$supplementary_gid" =~ ^[0-9]+$ || "$supplementary_gid" != "$group_gid" ]]; then
			fail "existing stardust account is unsafe; it must not have supplementary groups"
		fi
	done
}

prepare_linux_service_inventory() {
	local unit_path="$1" unit_files candidate candidate_index token policy_tmp configured_unit directory_info policy_info expected_exec_start
	local service_inventory_requested=false
	local -a selected_units=()

	if [[ "$enable_service_inventory" != true && ! -e "$service_policy_path" && ! -L "$service_policy_path" ]]; then
		return
	fi

	if [[ -e "$service_policy_dir" || -L "$service_policy_dir" ]]; then
		[[ -d "$service_policy_dir" && ! -L "$service_policy_dir" ]] || fail "service policy directory must be a real directory"
		directory_info="$(stat -c '%U:%G:%a' "$service_policy_dir")" || fail "could not inspect the service policy directory"
		[[ "$directory_info" == root:stardust:750 ]] || fail "existing service policy directory must be root:stardust mode 0750"
	else
		mkdir -p "$service_policy_dir" || fail "could not create the service policy directory"
		chown root:stardust "$service_policy_dir" || fail "could not assign the service policy directory to root:stardust"
		chmod 0750 "$service_policy_dir" || fail "could not restrict the service policy directory"
	fi

	if [[ -L "$service_policy_path" || ( -e "$service_policy_path" && ! -f "$service_policy_path" ) ]]; then
		fail "service policy must be a regular file, not a symbolic link or special file"
	fi

	if [[ -e "$service_policy_path" ]]; then
		policy_info="$(stat -c '%U:%a' "$service_policy_path")" || fail "could not inspect the service policy file"
		[[ "$policy_info" == stardust:400 || "$policy_info" == stardust:600 ]] || \
			fail "existing service policy must be owned by stardust with mode 0400 or 0600"
	else
		[[ "$enable_service_inventory" == true ]] || fail "service policy file is missing"
		unit_files="$(LC_ALL=C systemctl list-unit-files --type=service --no-legend --no-pager)" || \
			fail "could not list installed systemd services for the default allowlist"
		for candidate in "${default_service_units[@]}"; do
			if awk -v target="$candidate" '$1 == target { found = 1 } END { exit !found }' <<<"$unit_files"; then
				selected_units+=("$candidate")
			fi
		done
		((${#selected_units[@]} > 0)) || fail "none of the common default services are installed; no service inventory policy was created"

		token="$(openssl rand -hex 32)" || fail "could not generate a service inventory token"
		[[ "$token" =~ ^[[:xdigit:]]{64}$ ]] || fail "generated service inventory token was invalid"
		policy_tmp="${tmp_dir}/services.json"
		{
			printf '{\n  "token": "%s",\n  "units": [\n' "$token"
			for candidate_index in "${!selected_units[@]}"; do
				if ((candidate_index + 1 < ${#selected_units[@]})); then
					printf '    "%s",\n' "${selected_units[$candidate_index]}"
				else
					printf '    "%s"\n' "${selected_units[$candidate_index]}"
				fi
			done
			printf '  ]\n}\n'
		} > "$policy_tmp"
		install -o stardust -g stardust -m 0400 "$policy_tmp" "$service_policy_path" || \
			fail "could not create the private service inventory policy"
		service_inventory_requested=true
	fi

	configured_unit="${tmp_dir}/stardust.service.with-service-inventory"
	expected_exec_start="ExecStart=${install_dir}/stardust --listen 127.0.0.1:8787"
	awk -v policy="$service_policy_path" -v expected_start="$expected_exec_start" '
		/^\[Service\]$/ { in_service = 1; print; next }
		/^\[/ { in_service = 0 }
		in_service && /^ExecStart=/ {
			if (found || $0 != expected_start) exit 1
			print $0 " --service-policy-file=" policy
			found = 1
			next
		}
		{ print }
		END { if (!found) exit 1 }
	' "$unit_path" > "$configured_unit" || fail "could not safely add the service policy to the systemd unit"
	mv "$configured_unit" "$unit_path" || fail "could not prepare the systemd service policy"
	if [[ "$service_inventory_requested" == true ]]; then
		printf 'Enabled authenticated service inventory for installed units: %s\n' "${selected_units[*]}"
		printf 'The private read token is in %s; the installer does not print it.\n' "$service_policy_path"
	else
		printf 'Preserving and using the existing service inventory policy at %s\n' "$service_policy_path"
	fi
}

macos_id_in_service_range() {
	[[ "$1" =~ ^[0-9]+$ ]] || return 1
	((10#$1 >= 400 && 10#$1 <= 499))
}

macos_id_used_by_other() {
	local records="$1" identifier="$2" allowed_name="$3"
	awk -v identifier="$identifier" -v allowed_name="$allowed_name" \
		'$2 == identifier && $1 != allowed_name { used = 1 } END { exit !used }' <<<"$records"
}

macos_choose_service_id() {
	local records="$1" candidate
	for candidate in {400..499}; do
		if ! awk -v identifier="$candidate" \
			'$2 == identifier { used = 1 } END { exit used ? 0 : 1 }' <<<"$records"; then
			printf '%s' "$candidate"
			return 0
		fi
	done
	return 1
}

macos_dscl_attribute() {
	local record="$1" attribute="$2" output value
	output="$(dscl . -read "$record" "$attribute" 2>/dev/null)" || return 1
	value="$(awk -v attribute="${attribute}:" \
		'$1 == attribute { $1 = ""; sub(/^[[:space:]]+/, ""); print; exit }' <<<"$output")"
	[[ -n "$value" ]] || return 1
	printf '%s' "$value"
}

macos_verify_no_supplementary_groups() {
	local user="$1" primary_group="$2" generated_uid="$3" group_uuid="$4"
	local attribute value values membership_groups membership_group
	for attribute in GroupMembership GroupMembers NestedGroups; do
		case "$attribute" in
			GroupMembership) values="$user" ;;
			GroupMembers) values="$generated_uid $group_uuid" ;;
			NestedGroups) values="$group_uuid" ;;
		esac
		for value in $values; do
			membership_groups="$(dscl . -search /Groups "$attribute" "$value" | \
				awk -F '\t' -v expected="$attribute = (" '$3 == expected { print $1 }')" || \
				fail "could not inspect supplementary groups for _stardust"
			for membership_group in $membership_groups; do
				[[ "$membership_group" == "$primary_group" ]] || \
					fail "existing _stardust account has supplementary group membership in ${membership_group}"
			done
		done
	done
}

ensure_macos_service_account() {
	local user_record group_record users groups user_exists=false group_exists=false
	local user_uid user_gid group_gid user_shell user_home user_auth user_uuid group_uuid group_details group_members group_uuid_members group_nested_members
	local member
	local user=_stardust group=_stardust
	user_record="/Users/${user}"
	group_record="/Groups/${group}"

	users="$(dscl . -list /Users UniqueID)" || fail "could not inspect macOS user IDs"
	groups="$(dscl . -list /Groups PrimaryGroupID)" || fail "could not inspect macOS group IDs"
	if ! awk 'NF != 2 || $2 !~ /^[0-9]+$/ { invalid = 1 } END { exit invalid || NR == 0 }' <<<"$users"; then
		fail "could not safely parse macOS user IDs"
	fi
	if ! awk 'NF != 2 || $2 !~ /^[0-9]+$/ { invalid = 1 } END { exit invalid || NR == 0 }' <<<"$groups"; then
		fail "could not safely parse macOS group IDs"
	fi

	if dscl . -read "$user_record" >/dev/null 2>&1; then user_exists=true; fi
	if dscl . -read "$group_record" >/dev/null 2>&1; then group_exists=true; fi
	if [[ "$user_exists" == true && "$group_exists" != true ]]; then
		fail "existing _stardust user has no matching service group; resolve the account conflict manually"
	fi

	if [[ "$group_exists" == true ]]; then
		group_gid="$(macos_dscl_attribute "$group_record" PrimaryGroupID)" || fail "existing _stardust group has no valid primary group ID"
		group_uuid="$(macos_dscl_attribute "$group_record" GeneratedUID)" || fail "existing _stardust group has no generated UUID"
		macos_id_in_service_range "$group_gid" || fail "existing _stardust group is unsafe; its ID must be between 400 and 499"
		[[ "$group_uuid" =~ ^[[:xdigit:]]{8}-([[:xdigit:]]{4}-){3}[[:xdigit:]]{12}$ ]] || fail "existing _stardust group has an invalid generated UUID"
		if macos_id_used_by_other "$groups" "$group_gid" "$group"; then
			fail "existing _stardust group ID is already assigned to another group"
		fi
		group_details="$(dscl . -read "$group_record")" || fail "could not inspect the existing _stardust group"
		group_members="$(awk -F': ' '$1 == "GroupMembership" { print $2 }' <<<"$group_details")"
		for member in $group_members; do
			[[ "$member" == "$user" ]] || fail "existing _stardust group contains other users"
		done
		group_nested_members="$(awk -F': ' '$1 == "NestedGroups" { print $2 }' <<<"$group_details")"
		[[ -z "$group_nested_members" ]] || fail "existing _stardust group must not contain nested groups"
	else
		group_gid="$(macos_choose_service_id "$groups")" || fail "no unused macOS system group ID is available between 400 and 499"
		group_uuid="$(uuidgen)" || fail "could not generate a UUID for the _stardust service group"
		[[ "$group_uuid" =~ ^[[:xdigit:]]{8}-([[:xdigit:]]{4}-){3}[[:xdigit:]]{12}$ ]] || fail "uuidgen returned an invalid UUID for the _stardust service group"
	fi

	if [[ "$user_exists" == true ]]; then
		user_uid="$(macos_dscl_attribute "$user_record" UniqueID)" || fail "existing _stardust user has no valid user ID"
		user_gid="$(macos_dscl_attribute "$user_record" PrimaryGroupID)" || fail "existing _stardust user has no primary group"
		user_shell="$(macos_dscl_attribute "$user_record" UserShell)" || fail "existing _stardust user has no login shell setting"
		user_home="$(macos_dscl_attribute "$user_record" NFSHomeDirectory)" || fail "existing _stardust user has no home setting"
		user_auth="$(macos_dscl_attribute "$user_record" AuthenticationAuthority)" || fail "existing _stardust user authentication is not disabled"
		user_uuid="$(macos_dscl_attribute "$user_record" GeneratedUID)" || fail "existing _stardust user has no generated UUID"
		macos_id_in_service_range "$user_uid" || fail "existing _stardust user is unsafe; its ID must be between 400 and 499"
		[[ "$user_gid" == "$group_gid" ]] || fail "existing _stardust user must use the _stardust primary group"
		if macos_id_used_by_other "$users" "$user_uid" "$user"; then
			fail "existing _stardust user ID is already assigned to another user"
		fi
		[[ "$user_shell" == /usr/bin/false ]] || fail "existing _stardust user must have a non-login shell"
		[[ "$user_home" == /var/empty ]] || fail "existing _stardust user must not have a personal home directory"
		[[ "$user_auth" == ';DisabledUser;' ]] || fail "existing _stardust user authentication must be disabled"
		[[ "$user_uuid" =~ ^[[:xdigit:]]{8}-([[:xdigit:]]{4}-){3}[[:xdigit:]]{12}$ ]] || fail "existing _stardust user has an invalid generated UUID"
	else
		user_uid="$(macos_choose_service_id "$users")" || fail "no unused macOS system user ID is available between 400 and 499"
		user_uuid="$(uuidgen)" || fail "could not generate a UUID for the _stardust service user"
		[[ "$user_uuid" =~ ^[[:xdigit:]]{8}-([[:xdigit:]]{4}-){3}[[:xdigit:]]{12}$ ]] || fail "uuidgen returned an invalid UUID for the _stardust service user"
	fi
	if [[ "$group_exists" == true ]]; then
		group_details="$(dscl . -read "$group_record")" || fail "could not inspect the _stardust service group"
		group_uuid_members="$(awk -F': ' '$1 == "GroupMembers" { print $2 }' <<<"$group_details")"
		for member in $group_uuid_members; do
			[[ "$member" == "$user_uuid" ]] || fail "existing _stardust group contains other users"
		done
	fi

	if [[ "$group_exists" != true ]]; then
		dscl . -create "$group_record" || fail "could not create the _stardust service group"
		dscl . -create "$group_record" PrimaryGroupID "$group_gid" || fail "could not set the _stardust service group ID"
		dscl . -create "$group_record" GeneratedUID "$group_uuid" || fail "could not set the _stardust group UUID"
		dscl . -create "$group_record" RealName "Stardust service group" || fail "could not name the _stardust service group"
	fi
	if [[ "$user_exists" != true ]]; then
		dscl . -create "$user_record" || fail "could not create the _stardust service user"
		dscl . -create "$user_record" UniqueID "$user_uid" || fail "could not set the _stardust user ID"
		dscl . -create "$user_record" GeneratedUID "$user_uuid" || fail "could not set the _stardust user UUID"
		dscl . -create "$user_record" PrimaryGroupID "$group_gid" || fail "could not set the _stardust primary group"
		dscl . -create "$user_record" RealName "Stardust service account" || fail "could not name the _stardust service user"
		dscl . -create "$user_record" UserShell /usr/bin/false || fail "could not disable the _stardust login shell"
		dscl . -create "$user_record" NFSHomeDirectory /var/empty || fail "could not disable the _stardust home directory"
		dscl . -create "$user_record" AuthenticationAuthority ';DisabledUser;' || fail "could not disable _stardust authentication"
		dscl . -create "$user_record" IsHidden 1 || fail "could not hide the _stardust service account"
	fi
	macos_verify_no_supplementary_groups "$user" "$group" "$user_uuid" "$group_uuid"

	launchd_user="$user"
	launchd_group="$group"
}

prepare_macos_log_directory() {
	local log_file link_count
	mkdir -p "$launchd_log_dir" || fail "could not create the Stardust log directory"
	[[ -d "$launchd_log_dir" && ! -L "$launchd_log_dir" ]] || fail "the Stardust log path must be a real directory, not a symlink"
	chown "$launchd_user:$launchd_group" "$launchd_log_dir" || fail "could not assign the Stardust log directory to ${launchd_user}"
	chmod 0750 "$launchd_log_dir" || fail "could not restrict the Stardust log directory"
	for log_file in "${launchd_log_dir}/stdout.log" "${launchd_log_dir}/stderr.log"; do
		[[ ! -L "$log_file" ]] || fail "Stardust log file ${log_file} must not be a symlink"
		if [[ -e "$log_file" ]]; then
			[[ -f "$log_file" ]] || fail "Stardust log path ${log_file} must be a regular file"
			link_count="$(stat -f '%l' "$log_file")" || fail "could not inspect Stardust log file ${log_file}"
			[[ "$link_count" == 1 ]] || fail "Stardust log file ${log_file} must not have hard links"
			chown "$launchd_user:$launchd_group" "$log_file" || fail "could not assign Stardust log file ${log_file} to ${launchd_user}"
			chmod 0640 "$log_file" || fail "could not restrict Stardust log file ${log_file}"
		fi
	done
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
	--enable-service-inventory)
		enable_service_inventory=true
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
if [[ "$enable_service_inventory" == true && ( "$install_service" != true || "$service_manager" != systemd ) ]]; then
	fail "--enable-service-inventory requires a Linux systemd service install"
fi

case "$(uname -m)" in
x86_64|amd64) arch=amd64 ;;
aarch64|arm64) arch=arm64 ;;
*) fail "unsupported architecture: $(uname -m) (supported: x86_64, aarch64)" ;;
esac

for tool in curl tar awk install mktemp openssl; do
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
		for tool in systemctl systemd-analyze getent groupadd useradd id stat; do
			command -v "$tool" >/dev/null 2>&1 || fail "required command not found: $tool (or pass --no-service)"
		done
		systemctl show-environment >/dev/null 2>&1 || fail "systemd is not running; pass --no-service to install only the binary"
	else
		(( EUID == 0 )) || fail "macOS LaunchDaemon installation requires sudo; rerun with sudo or pass --no-service"
		for tool in launchctl plutil chown chmod dscl stat uuidgen; do
			command -v "$tool" >/dev/null 2>&1 || fail "required command not found: $tool (or pass --no-service)"
		done
		launchd_domain=system
		launchd_label=com.ch55secake.stardust
		launchd_target="${launchd_domain}/${launchd_label}"
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
signature_path="${tmp_dir}/checksums.txt.sig"
curl --fail --location --silent --show-error --proto '=https' --tlsv1.2 \
	--output "$signature_path" "${release_url}/checksums.txt.sig" || fail "could not download signed release checksums"
signature_verified=false
for key_index in "${!trusted_release_public_keys[@]}"; do
	trusted_key_path="${tmp_dir}/release-public-key-${key_index}.pem"
	printf '%s\n' "${trusted_release_public_keys[$key_index]}" > "$trusted_key_path"
	if openssl dgst -sha256 -verify "$trusted_key_path" -signature "$signature_path" "$checksums_path" >/dev/null 2>&1; then
		signature_verified=true
		break
	fi
done
[[ "$signature_verified" == true ]] || fail "release checksum signature verification failed"

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
		ensure_linux_service_account
		prepare_linux_service_inventory "$unit_path"
	else
		plist_template="${tmp_dir}/com.ch55secake.stardust.template.plist"
		plist_path="${tmp_dir}/com.ch55secake.stardust.plist"
		curl --fail --location --silent --show-error --proto '=https' --tlsv1.2 \
			--output "$plist_template" "https://raw.githubusercontent.com/${repo}/${release_tag}/deploy/launchd/${launchd_label}.plist" || fail "could not download the LaunchDaemon plist for ${release_tag}"
		ensure_macos_service_account
		cp "$plist_template" "$plist_path"
		plutil -replace ProgramArguments -array "$plist_path" || fail "could not reset the LaunchDaemon arguments"
		plutil -insert ProgramArguments -string "${install_dir}/stardust" -append "$plist_path" || fail "could not configure the LaunchDaemon binary path"
		plutil -insert ProgramArguments -string --listen -append "$plist_path" || fail "could not configure the LaunchDaemon listener option"
		plutil -insert ProgramArguments -string 127.0.0.1:8787 -append "$plist_path" || fail "could not configure the LaunchDaemon listener address"
		plutil -replace UserName -string "$launchd_user" "$plist_path" || fail "could not configure the LaunchDaemon user"
		plutil -replace GroupName -string "$launchd_group" "$plist_path" || fail "could not configure the LaunchDaemon group"
		plutil -replace StandardOutPath -string "${launchd_log_dir}/stdout.log" "$plist_path" || fail "could not configure the LaunchDaemon log path"
		plutil -replace StandardErrorPath -string "${launchd_log_dir}/stderr.log" "$plist_path" || fail "could not configure the LaunchDaemon error log path"
		plutil -lint "$plist_path" || fail "the LaunchDaemon plist failed validation"
	fi
fi

mkdir -p "$install_dir" || fail "cannot create ${install_dir}; choose a writable directory with --install-dir"
staged_path="$(mktemp "${install_dir}/.stardust.XXXXXX")" || fail "cannot write to ${install_dir}; choose a writable directory with --install-dir"
if [[ "$goos" == darwin && "$install_service" == true ]]; then
	install -o root -g wheel -m 0755 "${tmp_dir}/stardust" "$staged_path" || fail "could not set root ownership on the Stardust binary"
else
	install -m 0755 "${tmp_dir}/stardust" "$staged_path" || fail "could not install the Stardust binary"
fi
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
		prepare_macos_log_directory
		launchctl bootout "$launchd_target" >/dev/null 2>&1 || true
		install -o root -g wheel -m 0644 "$plist_path" "/Library/LaunchDaemons/${launchd_label}.plist" || fail "could not install the LaunchDaemon plist"
		launchctl enable "$launchd_target" || fail "could not enable the Stardust LaunchDaemon"
		launchctl bootstrap "$launchd_domain" "/Library/LaunchDaemons/${launchd_label}.plist" || fail "could not start the Stardust LaunchDaemon"
		printf 'Enabled and started LaunchDaemon %s as %s. Logs are in %s\n' "$launchd_label" "$launchd_user" "$launchd_log_dir"
	fi
fi
