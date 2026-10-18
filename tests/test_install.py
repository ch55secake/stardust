import hashlib
import io
import json
import os
import pathlib
import plistlib
import re
import shutil
import subprocess
import tarfile
import tempfile
import unittest


REPO_ROOT = pathlib.Path(__file__).resolve().parents[1]
INSTALLER = REPO_ROOT / "install.sh"

FAKE_TOOL = r'''#!/usr/bin/env python3
import hashlib
import json
import os
import pathlib
import plistlib
import shutil
import sys

tool = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]

if tool == "uname":
    if args == ["-s"]:
        print(os.environ["STARDUST_TEST_OS"])
    elif args == ["-m"]:
        print(os.environ["STARDUST_TEST_ARCH"])
    else:
        sys.exit(2)
elif tool == "curl":
    output = args[args.index("--output") + 1]
    url = next(arg for arg in args if arg.startswith("https://"))
    with open(os.environ["STARDUST_CURL_LOG"], "a", encoding="utf-8") as log:
        log.write(url + "\n")
    fixtures = pathlib.Path(os.environ["STARDUST_FIXTURES"])
    if url.endswith("/checksums.txt"):
        name = "checksums-bad.txt" if os.environ.get("STARDUST_BAD_CHECKSUM") == "1" else "checksums.txt"
    elif url.endswith("/checksums.txt.sig"):
        if os.environ.get("STARDUST_MISSING_SIGNATURE") == "1":
            sys.exit(22)
        name = "checksums-bad.sig" if os.environ.get("STARDUST_BAD_SIGNATURE") == "1" else "checksums.txt.sig"
    else:
        name = url.rsplit("/", 1)[-1]
        if os.environ.get("STARDUST_FAIL_DOWNLOAD") == name:
            sys.exit(22)
    source = fixtures / name
    if name.endswith(".tar.gz") and os.environ.get("STARDUST_TAMPER_ARCHIVE") == name:
        source = fixtures / "tampered-archive.tar.gz"
    if not source.is_file():
        sys.exit(22)
    shutil.copyfile(source, output)
elif tool in ("sha256sum", "shasum"):
    file = args[-1]
    digest = hashlib.sha256(pathlib.Path(file).read_bytes()).hexdigest()
    print(f"{digest}  {file}")
elif tool == "getent":
    database, name = args
    state = pathlib.Path(os.environ["STARDUST_ACCOUNT_STATE"])
    mode = os.environ.get("STARDUST_ACCOUNT_MODE", "existing")
    if database == "group" and name == "stardust":
        if mode == "new" and not (state / "groupadd").exists():
            sys.exit(2)
        print(f"stardust:x:{os.environ['STARDUST_GROUP_GID']}:{os.environ['STARDUST_GROUP_MEMBERS']}")
    elif database == "passwd" and name == "stardust":
        if mode == "new" and not (state / "useradd").exists():
            sys.exit(2)
        print(
            ":".join(
                (
                    "stardust",
                    "x",
                    os.environ["STARDUST_USER_UID"],
                    os.environ["STARDUST_USER_GID"],
                    "Stardust service account",
                    os.environ["STARDUST_USER_HOME"],
                    os.environ["STARDUST_USER_SHELL"],
                )
            )
        )
    else:
        sys.exit(2)
elif tool in ("groupadd", "useradd"):
    state = pathlib.Path(os.environ["STARDUST_ACCOUNT_STATE"])
    state.mkdir(parents=True, exist_ok=True)
    with open(os.environ["STARDUST_ACCOUNT_LOG"], "a", encoding="utf-8") as log:
        log.write(tool + " " + " ".join(args) + "\n")
    (state / tool).touch()
elif tool == "id":
    if args == ["-G", "stardust"]:
        print(os.environ["STARDUST_GROUP_IDS"])
    else:
        sys.exit(2)
elif tool == "dscl":
    state_path = pathlib.Path(os.environ["STARDUST_DSCL_STATE"])
    state_path.parent.mkdir(parents=True, exist_ok=True)
    if state_path.exists():
        state = json.loads(state_path.read_text(encoding="utf-8"))
    else:
        state = json.loads(os.environ["STARDUST_DSCL_INITIAL"])
        state_path.write_text(json.dumps(state), encoding="utf-8")
    with open(os.environ["STARDUST_DSCL_LOG"], "a", encoding="utf-8") as log:
        log.write(" ".join(args) + "\n")
    if args[:2] == [".", "-list"]:
        collection = "users" if args[2] == "/Users" else "groups"
        attribute = "UniqueID" if collection == "users" else "PrimaryGroupID"
        for name, values in sorted(state[collection].items()):
            print(f"{name} {values[attribute]}")
    elif args[:2] == [".", "-search"]:
        if args[2] != "/Groups":
            sys.exit(2)
        attribute, target = args[3], args[4]
        for name, values in sorted(state["groups"].items()):
            if target in values.get(attribute, "").split():
                print(f"{name}\t\t{attribute} = (")
                for member in values[attribute].split():
                    print(f"    {member},")
                print(")")
    elif args[:2] == [".", "-read"]:
        collection, name = args[2].split("/", 2)[1:]
        records = state["users"] if collection == "Users" else state["groups"]
        if name not in records:
            sys.exit(2)
        values = records[name]
        if len(args) == 4:
            attribute = args[3]
            if attribute not in values:
                sys.exit(2)
            print(f"{attribute}: {values[attribute]}")
        else:
            for attribute, value in sorted(values.items()):
                print(f"{attribute}: {value}")
    elif args[:2] == [".", "-create"]:
        collection, name = args[2].split("/", 2)[1:]
        records = state["users"] if collection == "Users" else state["groups"]
        values = records.setdefault(name, {})
        if len(args) > 3:
            values[args[3]] = " ".join(args[4:])
        state_path.write_text(json.dumps(state), encoding="utf-8")
    else:
        sys.exit(2)
elif tool == "install":
    positionals = []
    mode = None
    index = 0
    while index < len(args):
        if args[index] in ("-m", "-o", "-g"):
            option = args[index]
            if option == "-m":
                mode = int(args[index + 1], 8)
            index += 2
        else:
            positionals.append(args[index])
            index += 1
    if len(positionals) != 2:
        sys.exit(2)
    shutil.copyfile(positionals[0], positionals[1])
    if mode is not None:
        os.chmod(positionals[1], mode)
    with open(os.environ["STARDUST_INSTALL_LOG"], "a", encoding="utf-8") as log:
        log.write(" ".join(args) + "\n")
elif tool == "plutil":
    with open(os.environ["STARDUST_PLUTIL_LOG"], "a", encoding="utf-8") as log:
        log.write(" ".join(args) + "\n")
    if args[0] == "-lint":
        with open(args[1], "rb") as source:
            plistlib.load(source)
        print(f"{args[1]}: OK")
    elif args[0] == "-replace":
        plist_path = pathlib.Path(args[-1])
        plist = plistlib.loads(plist_path.read_bytes())
        if args[2] == "-array":
            plist[args[1]] = []
        elif args[2] == "-string":
            plist[args[1]] = args[3]
        else:
            sys.exit(2)
        plist_path.write_bytes(plistlib.dumps(plist))
    elif args[0] == "-insert":
        plist_path = pathlib.Path(args[-1])
        plist = plistlib.loads(plist_path.read_bytes())
        plist[args[1]].append(args[3])
        plist_path.write_bytes(plistlib.dumps(plist))
    else:
        sys.exit(2)
elif tool == "launchctl":
    with open(os.environ["STARDUST_LAUNCHCTL_LOG"], "a", encoding="utf-8") as log:
        log.write(" ".join(args) + "\n")
elif tool in ("chown", "chmod"):
    with open(os.environ["STARDUST_OWNERSHIP_LOG"], "a", encoding="utf-8") as log:
        log.write(tool + " " + " ".join(args) + "\n")
elif tool == "stat":
    if args[:2] == ["-f", "%l"]:
        print(pathlib.Path(args[2]).stat().st_nlink)
    else:
        sys.exit(2)
elif tool == "uuidgen":
    counter_path = pathlib.Path(os.environ["STARDUST_UUID_COUNTER"])
    counter = int(counter_path.read_text()) if counter_path.exists() else 0
    counter += 1
    counter_path.write_text(str(counter), encoding="utf-8")
    print(os.environ[f"STARDUST_UUID_{counter}"])
elif tool == "systemctl":
    with open(os.environ["STARDUST_SYSTEMCTL_LOG"], "a", encoding="utf-8") as log:
        log.write(" ".join(args) + "\n")
    if args and args[0] == "is-active":
        sys.exit(1)
elif tool == "systemd-analyze":
    pass
else:
    sys.exit(2)
'''


class InstallerRegressionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="stardust-installer-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.fixtures = self.root / "fixtures"
        self.stubs = self.root / "stubs"
        self.fixtures.mkdir()
        self.stubs.mkdir()
        self.curl_log = self.root / "curl.log"
        self.signing_key = self.root / "test-signing-key.pem"
        self.public_key = self.root / "test-release-public-key.pem"
        subprocess.run(
            ["openssl", "genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048", "-out", str(self.signing_key)],
            check=True,
            capture_output=True,
        )
        subprocess.run(
            ["openssl", "pkey", "-in", str(self.signing_key), "-pubout", "-out", str(self.public_key)],
            check=True,
            capture_output=True,
        )
        self.public_key_text = self.public_key.read_text(encoding="utf-8").strip()
        self.assets = {}
        checksums = []

        for goos in ("linux", "darwin"):
            for goarch in ("amd64", "arm64"):
                name = f"stardust_1.2.3_{goos}_{goarch}.tar.gz"
                payload = f"fixture binary for {goos}/{goarch}".encode()
                archive_path = self.fixtures / name
                with tarfile.open(archive_path, "w:gz") as archive:
                    info = tarfile.TarInfo("stardust")
                    info.size = len(payload)
                    info.mode = 0o755
                    archive.addfile(info, io.BytesIO(payload))
                digest = hashlib.sha256(archive_path.read_bytes()).hexdigest()
                checksums.append(f"{digest}  {name}")
                self.assets[(goos, goarch)] = (name, payload)

        (self.fixtures / "checksums.txt").write_text("\n".join(checksums) + "\n")
        subprocess.run(
            [
                "openssl", "dgst", "-sha256", "-sign", str(self.signing_key),
                "-out", str(self.fixtures / "checksums.txt.sig"), str(self.fixtures / "checksums.txt"),
            ],
            check=True,
            capture_output=True,
        )
        bad_signature = bytearray((self.fixtures / "checksums.txt.sig").read_bytes())
        bad_signature[0] ^= 1
        (self.fixtures / "checksums-bad.sig").write_bytes(bad_signature)
        bad_checksums = []
        for entry in checksums:
            digest, name = entry.split()
            replacement = ("0" if digest[0] != "0" else "1") + digest[1:]
            bad_checksums.append(f"{replacement}  {name}")
        (self.fixtures / "checksums-bad.txt").write_text("\n".join(bad_checksums) + "\n")
        (self.fixtures / "tampered-archive.tar.gz").write_bytes(b"tampered release archive")
        shutil.copyfile(REPO_ROOT / "deploy/systemd/stardust.service", self.fixtures / "stardust.service")
        shutil.copyfile(
            REPO_ROOT / "deploy/launchd/com.ch55secake.stardust.plist",
            self.fixtures / "com.ch55secake.stardust.plist",
        )

        fake_tool = self.stubs / "fake-tool"
        fake_tool.write_text(FAKE_TOOL)
        fake_tool.chmod(0o755)
        for name in (
            "uname",
            "curl",
            "sha256sum",
            "shasum",
            "getent",
            "groupadd",
            "useradd",
            "id",
            "systemctl",
            "systemd-analyze",
            "dscl",
            "launchctl",
            "plutil",
            "chown",
            "chmod",
            "stat",
            "install",
            "uuidgen",
        ):
            (self.stubs / name).symlink_to(fake_tool)

        self.test_installer = self.root / "install.sh"
        self.test_installer.write_text(self._installer_script(), encoding="utf-8")
        self.test_installer.chmod(0o755)

    def _installer_script(self):
        script = INSTALLER.read_text(encoding="utf-8")
        pattern = r"(?<=trusted_release_public_keys=\(\n')[\s\S]*?(?='\n\))"
        script, count = re.subn(pattern, lambda _: self.public_key_text, script, count=1)
        if count != 1:
            raise AssertionError("could not replace the release verification test key")
        return script

    def _env(self, goos="Linux", machine="x86_64", **overrides):
        env = os.environ.copy()
        env.update(
            {
                "PATH": f"{self.stubs}{os.pathsep}{env['PATH']}",
                "STARDUST_TEST_OS": goos,
                "STARDUST_TEST_ARCH": machine,
                "STARDUST_FIXTURES": str(self.fixtures),
                "STARDUST_CURL_LOG": str(self.curl_log),
                "STARDUST_SYSTEMCTL_LOG": str(self.root / "systemctl.log"),
                "STARDUST_DSCL_STATE": str(self.root / "dscl-state.json"),
                "STARDUST_DSCL_INITIAL": json.dumps(
                    {
                        "users": {"root": {"UniqueID": "0"}, "admin": {"UniqueID": "501"}},
                        "groups": {
                            "wheel": {"PrimaryGroupID": "0"},
                            "staff": {"PrimaryGroupID": "20"},
                            "admin": {"PrimaryGroupID": "80"},
                        },
                    }
                ),
                "STARDUST_DSCL_LOG": str(self.root / "dscl.log"),
                "STARDUST_PLUTIL_LOG": str(self.root / "plutil.log"),
                "STARDUST_LAUNCHCTL_LOG": str(self.root / "launchctl.log"),
                "STARDUST_OWNERSHIP_LOG": str(self.root / "ownership.log"),
                "STARDUST_INSTALL_LOG": str(self.root / "install.log"),
                "STARDUST_UUID_COUNTER": str(self.root / "uuidgen-count"),
                "STARDUST_UUID_1": "11111111-2222-4333-8444-555555555555",
                "STARDUST_UUID_2": "66666666-7777-4888-8999-aaaaaaaaaaaa",
                "TMPDIR": str(self.root),
            }
        )
        env.update(overrides)
        return env

    def _run_installer(self, install_dir, *, version=None, env=None):
        command = ["bash", str(self.test_installer), "--no-service", "--install-dir", str(install_dir)]
        if version is not None:
            command.extend(("--version", version))
        return subprocess.run(
            command,
            cwd=REPO_ROOT,
            env=env or self._env(),
            capture_output=True,
            text=True,
            check=False,
        )

    def _run_linux_service_installer(self, name, *, account_mode="existing", existing_binary=None, **account):
        case_dir = self.root / name
        case_dir.mkdir()
        install_dir = case_dir / "usr" / "local" / "bin"
        if existing_binary is not None:
            install_dir.mkdir(parents=True)
            (install_dir / "stardust").write_bytes(existing_binary)
        unit_path = case_dir / "etc" / "systemd" / "system" / "stardust.service"
        unit_path.parent.mkdir(parents=True)
        state = case_dir / "account-state"
        state.mkdir()
        account_log = case_dir / "account.log"

        script = self._installer_script()
        root_check = '\t\t(( EUID == 0 )) || fail "systemd installation requires root; use sudo or pass --no-service"\n'
        self.assertIn(root_check, script)
        # Run the service path without root; all privileged writes target this temp directory.
        script = script.replace(root_check, "\t\t:\n", 1)
        script = script.replace("/usr/local/bin", str(install_dir))
        script = script.replace("/etc/systemd/system/stardust.service", str(unit_path))
        test_installer = case_dir / "install.sh"
        test_installer.write_text(script)
        test_installer.chmod(0o755)

        account_env = {
            "STARDUST_ACCOUNT_MODE": account_mode,
            "STARDUST_ACCOUNT_STATE": str(state),
            "STARDUST_ACCOUNT_LOG": str(account_log),
            "STARDUST_GROUP_GID": "700",
            "STARDUST_GROUP_MEMBERS": "",
            "STARDUST_USER_UID": "701",
            "STARDUST_USER_GID": "700",
            "STARDUST_USER_HOME": "/nonexistent",
            "STARDUST_USER_SHELL": "/usr/sbin/nologin",
            "STARDUST_GROUP_IDS": "700",
        }
        account_env.update(account)
        env = self._env("Linux", "x86_64", **account_env)
        result = subprocess.run(
            ["bash", str(test_installer), "--version", "v1.2.3"],
            cwd=REPO_ROOT,
            env=env,
            capture_output=True,
            text=True,
            check=False,
        )
        return result, install_dir, unit_path, state, account_log

    def _run_macos_service_installer(self, name, *, existing_binary=None, initial_state=None, **overrides):
        case_dir = self.root / name
        case_dir.mkdir(exist_ok=True)
        install_dir = case_dir / "usr" / "local" / "bin"
        if existing_binary is not None:
            install_dir.mkdir(parents=True)
            (install_dir / "stardust").write_bytes(existing_binary)
        launchd_dir = case_dir / "Library" / "LaunchDaemons"
        launchd_dir.mkdir(parents=True)
        log_dir = case_dir / "Library" / "Logs" / "Stardust"
        plist_path = launchd_dir / "com.ch55secake.stardust.plist"

        script = self._installer_script()
        root_check = '\t\t(( EUID == 0 )) || fail "macOS LaunchDaemon installation requires sudo; rerun with sudo or pass --no-service"\n'
        self.assertIn(root_check, script)
        # Exercise service setup with privileged destinations redirected into this test's temp directory.
        script = script.replace(root_check, "\t\t:\n", 1)
        script = script.replace("/usr/local/bin", str(install_dir))
        script = script.replace("/Library/LaunchDaemons", str(launchd_dir))
        script = script.replace("/Library/Logs/Stardust", str(log_dir))
        test_installer = case_dir / "install.sh"
        test_installer.write_text(script)
        test_installer.chmod(0o755)

        env = self._env(
            "Darwin",
            "x86_64",
            SUDO_USER="admin",
            STARDUST_DSCL_STATE=str(case_dir / "dscl-state.json"),
            STARDUST_DSCL_LOG=str(case_dir / "dscl.log"),
            STARDUST_PLUTIL_LOG=str(case_dir / "plutil.log"),
            STARDUST_LAUNCHCTL_LOG=str(case_dir / "launchctl.log"),
            STARDUST_OWNERSHIP_LOG=str(case_dir / "ownership.log"),
            STARDUST_INSTALL_LOG=str(case_dir / "install.log"),
            STARDUST_UUID_COUNTER=str(case_dir / "uuidgen-count"),
            STARDUST_DSCL_INITIAL=json.dumps(initial_state)
            if initial_state is not None
            else self._env()["STARDUST_DSCL_INITIAL"],
            **overrides,
        )
        result = subprocess.run(
            ["bash", str(test_installer), "--version", "v1.2.3"],
            cwd=REPO_ROOT,
            env=env,
            capture_output=True,
            text=True,
            check=False,
        )
        return result, install_dir, plist_path, log_dir, env

    def test_selects_os_arch_assets_for_latest_and_pinned_releases(self):
        cases = (
            ("Linux", "x86_64", "linux", "amd64", None),
            ("Linux", "aarch64", "linux", "arm64", "v1.2.3"),
            ("Darwin", "x86_64", "darwin", "amd64", "v1.2.3"),
            ("Darwin", "arm64", "darwin", "arm64", None),
        )
        for goos, machine, asset_os, asset_arch, version in cases:
            with self.subTest(goos=goos, machine=machine, version=version):
                install_dir = self.root / f"install-{asset_os}-{asset_arch}"
                result = self._run_installer(install_dir, version=version, env=self._env(goos, machine))
                self.assertEqual(result.returncode, 0, result.stderr)
                name, payload = self.assets[(asset_os, asset_arch)]
                installed = install_dir / "stardust"
                self.assertEqual(installed.read_bytes(), payload)
                self.assertTrue(os.access(installed, os.X_OK))
                requests = self.curl_log.read_text().splitlines()
                release_path = f"/releases/download/{version}/" if version else "/releases/latest/download/"
                self.assertIn(f"https://github.com/ch55secake/stardust{release_path}checksums.txt", requests)
                self.assertIn(f"https://github.com/ch55secake/stardust{release_path}checksums.txt.sig", requests)
                self.assertIn(f"https://github.com/ch55secake/stardust{release_path}{name}", requests)

    def test_macos_service_install_creates_a_dedicated_unprivileged_account(self):
        result, install_dir, plist_path, log_dir, env = self._run_macos_service_installer(
            "macos-dedicated-account"
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        state = json.loads(pathlib.Path(env["STARDUST_DSCL_STATE"]).read_text())
        self.assertEqual(state["groups"]["_stardust"]["PrimaryGroupID"], "400")
        self.assertEqual(state["users"]["_stardust"]["UniqueID"], "400")
        self.assertEqual(
            state["users"]["_stardust"]["GeneratedUID"],
            "66666666-7777-4888-8999-aaaaaaaaaaaa",
        )
        self.assertEqual(
            state["groups"]["_stardust"]["GeneratedUID"],
            "11111111-2222-4333-8444-555555555555",
        )
        self.assertEqual(state["users"]["_stardust"]["PrimaryGroupID"], "400")
        self.assertEqual(state["users"]["_stardust"]["UserShell"], "/usr/bin/false")
        self.assertEqual(state["users"]["_stardust"]["NFSHomeDirectory"], "/var/empty")
        self.assertEqual(state["users"]["_stardust"]["AuthenticationAuthority"], ";DisabledUser;")
        self.assertEqual(state["users"]["_stardust"]["IsHidden"], "1")

        plist = plistlib.loads(plist_path.read_bytes())
        self.assertEqual(plist["UserName"], "_stardust")
        self.assertEqual(plist["GroupName"], "_stardust")
        self.assertEqual(
            plist["ProgramArguments"],
            [str(install_dir / "stardust"), "--listen", "127.0.0.1:8787"],
        )
        install_commands = pathlib.Path(env["STARDUST_INSTALL_LOG"]).read_text()
        self.assertIn("-o root -g wheel -m 0755", install_commands)
        self.assertIn("-o root -g wheel -m 0644", install_commands)
        ownership_commands = pathlib.Path(env["STARDUST_OWNERSHIP_LOG"]).read_text()
        self.assertIn(f"chown _stardust:_stardust {log_dir}", ownership_commands)
        self.assertIn(f"chmod 0750 {log_dir}", ownership_commands)
        launchctl_commands = pathlib.Path(env["STARDUST_LAUNCHCTL_LOG"]).read_text()
        self.assertIn("bootstrap system", launchctl_commands)

    def test_macos_upgrade_reuses_safe_account_and_reassigns_existing_logs(self):
        existing_user = {
            "UniqueID": "420",
            "GeneratedUID": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
            "PrimaryGroupID": "420",
            "UserShell": "/usr/bin/false",
            "NFSHomeDirectory": "/var/empty",
            "AuthenticationAuthority": ";DisabledUser;",
        }
        initial_state = {
            "users": {
                "root": {"UniqueID": "0"},
                "admin": {"UniqueID": "501"},
                "_stardust": existing_user,
            },
            "groups": {
                "wheel": {"PrimaryGroupID": "0"},
                "staff": {"PrimaryGroupID": "20"},
                "admin": {"PrimaryGroupID": "80"},
                "_stardust": {
                    "PrimaryGroupID": "420",
                    "GeneratedUID": "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
                },
            },
        }
        case_dir = self.root / "macos-safe-upgrade"
        log_dir = case_dir / "Library" / "Logs" / "Stardust"
        log_dir.mkdir(parents=True)
        (log_dir / "stdout.log").write_text("previous log data\n")
        result, _, _, _, env = self._run_macos_service_installer(
            "macos-safe-upgrade", initial_state=initial_state
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        dscl_commands = pathlib.Path(env["STARDUST_DSCL_LOG"]).read_text()
        self.assertNotIn(" -create ", dscl_commands)
        self.assertEqual((log_dir / "stdout.log").read_text(), "previous log data\n")
        ownership_commands = pathlib.Path(env["STARDUST_OWNERSHIP_LOG"]).read_text()
        self.assertIn(f"chown _stardust:_stardust {log_dir / 'stdout.log'}", ownership_commands)
        self.assertIn(f"chmod 0640 {log_dir / 'stdout.log'}", ownership_commands)

    def test_macos_no_service_does_not_create_or_inspect_service_account(self):
        result = self._run_installer(
            self.root / "macos-no-service",
            version="v1.2.3",
            env=self._env("Darwin", "arm64"),
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.root / "dscl.log").exists())
        self.assertFalse((self.root / "launchctl.log").exists())

    def test_macos_uid_conflict_and_orphaned_name_fail_without_overwriting_binary(self):
        valid_user = {
            "UniqueID": "420",
            "GeneratedUID": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
            "PrimaryGroupID": "420",
            "UserShell": "/usr/bin/false",
            "NFSHomeDirectory": "/var/empty",
            "AuthenticationAuthority": ";DisabledUser;",
        }
        cases = (
            (
                "uid-conflict",
                {
                    "users": {
                        "root": {"UniqueID": "0"},
                        "admin": {"UniqueID": "501"},
                        "_stardust": valid_user,
                        "other-service": {"UniqueID": "420"},
                    },
                    "groups": {
                        "wheel": {"PrimaryGroupID": "0"},
                        "_stardust": {
                            "PrimaryGroupID": "420",
                            "GeneratedUID": "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
                        },
                    },
                },
                "user ID is already assigned",
            ),
            (
                "orphaned-name",
                {
                    "users": {"root": {"UniqueID": "0"}, "_stardust": valid_user},
                    "groups": {"wheel": {"PrimaryGroupID": "0"}},
                },
                "no matching service group",
            ),
            (
                "supplementary-admin-group",
                {
                    "users": {
                        "root": {"UniqueID": "0"},
                        "admin": {"UniqueID": "501"},
                        "_stardust": valid_user,
                    },
                    "groups": {
                        "wheel": {"PrimaryGroupID": "0"},
                        "admin": {
                            "PrimaryGroupID": "80",
                            "GroupMembership": "_stardust",
                            "GroupMembers": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
                        },
                        "_stardust": {
                            "PrimaryGroupID": "420",
                            "GeneratedUID": "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
                        },
                    },
                },
                "supplementary group membership",
            ),
            (
                "nested-admin-group",
                {
                    "users": {
                        "root": {"UniqueID": "0"},
                        "admin": {"UniqueID": "501"},
                        "_stardust": valid_user,
                    },
                    "groups": {
                        "wheel": {"PrimaryGroupID": "0"},
                        "admin": {
                            "PrimaryGroupID": "80",
                            "NestedGroups": "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
                        },
                        "_stardust": {
                            "PrimaryGroupID": "420",
                            "GeneratedUID": "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
                        },
                    },
                },
                "supplementary group membership",
            ),
        )
        for name, initial_state, message in cases:
            with self.subTest(conflict=name):
                result, install_dir, _, _, _ = self._run_macos_service_installer(
                    f"macos-conflict-{name}",
                    existing_binary=b"existing binary",
                    initial_state=initial_state,
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertEqual((install_dir / "stardust").read_bytes(), b"existing binary")

    def test_tampered_checksums_do_not_replace_existing_install(self):
        install_dir = self.root / "existing-install"
        install_dir.mkdir()
        installed = install_dir / "stardust"
        installed.write_bytes(b"existing binary")

        result = self._run_installer(
            install_dir,
            version="v1.2.3",
            env=self._env(STARDUST_BAD_CHECKSUM="1"),
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("release checksum signature verification failed", result.stderr)
        requests = self.curl_log.read_text().splitlines()
        self.assertFalse(any(url.endswith(".tar.gz") for url in requests))
        self.assertEqual(installed.read_bytes(), b"existing binary")

    def test_missing_or_tampered_signature_does_not_replace_existing_install(self):
        for name, overrides, error in (
            ("missing-signature", {"STARDUST_MISSING_SIGNATURE": "1"}, "could not download signed release checksums"),
            ("tampered-signature", {"STARDUST_BAD_SIGNATURE": "1"}, "release checksum signature verification failed"),
        ):
            with self.subTest(name=name):
                install_dir = self.root / name
                install_dir.mkdir()
                installed = install_dir / "stardust"
                installed.write_bytes(b"existing binary")

                result = self._run_installer(
                    install_dir,
                    version="v1.2.3",
                    env=self._env(**overrides),
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertIn(error, result.stderr)
                requests = self.curl_log.read_text().splitlines()
                self.assertFalse(any(url.endswith(".tar.gz") for url in requests))
                self.assertEqual(installed.read_bytes(), b"existing binary")

    def test_tampered_archive_does_not_replace_existing_install(self):
        install_dir = self.root / "tampered-archive-install"
        install_dir.mkdir()
        installed = install_dir / "stardust"
        installed.write_bytes(b"existing binary")
        archive_name = self.assets[("linux", "amd64")][0]

        result = self._run_installer(
            install_dir,
            version="v1.2.3",
            env=self._env(STARDUST_TAMPER_ARCHIVE=archive_name),
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum verification failed", result.stderr)
        self.assertEqual(installed.read_bytes(), b"existing binary")

    def test_download_failure_does_not_replace_existing_install(self):
        install_dir = self.root / "existing-install"
        install_dir.mkdir()
        installed = install_dir / "stardust"
        installed.write_bytes(b"existing binary")
        archive_name = self.assets[("linux", "amd64")][0]

        result = self._run_installer(
            install_dir,
            version="v1.2.3",
            env=self._env(STARDUST_FAIL_DOWNLOAD=archive_name),
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("could not download", result.stderr)
        self.assertEqual(installed.read_bytes(), b"existing binary")

    def test_invalid_version_is_rejected_before_download(self):
        result = self._run_installer(self.root / "install", version="latest")

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("invalid version", result.stderr)
        self.assertFalse(self.curl_log.exists())

    def test_unsupported_os_and_architecture_are_rejected_before_download(self):
        cases = (
            ("FreeBSD", "x86_64", "only Linux and macOS"),
            ("Darwin", "riscv64", "unsupported architecture"),
        )
        for goos, machine, message in cases:
            with self.subTest(goos=goos, machine=machine):
                self.curl_log.unlink(missing_ok=True)
                result = self._run_installer(
                    self.root / f"unsupported-{goos}-{machine}",
                    env=self._env(goos, machine),
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertFalse(self.curl_log.exists())

    def test_linux_service_install_creates_a_restricted_account(self):
        result, install_dir, unit_path, state, account_log = self._run_linux_service_installer(
            "new-service-account", account_mode="new"
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((state / "groupadd").is_file())
        self.assertTrue((state / "useradd").is_file())
        account_commands = account_log.read_text()
        self.assertIn("groupadd --system stardust", account_commands)
        self.assertIn(
            "useradd --system --gid stardust --no-create-home --home-dir /nonexistent "
            "--shell /usr/sbin/nologin stardust",
            account_commands,
        )
        self.assertEqual(
            (install_dir / "stardust").read_bytes(),
            self.assets[("linux", "amd64")][1],
        )
        self.assertIn("start stardust.service", (self.root / "systemctl.log").read_text())
        unit = unit_path.read_text()
        self.assertIn("User=stardust", unit)
        self.assertIn("Group=stardust", unit)

    def test_linux_service_install_accepts_a_safe_existing_account(self):
        result, _, _, state, account_log = self._run_linux_service_installer("safe-existing-account")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((state / "groupadd").exists())
        self.assertFalse((state / "useradd").exists())
        self.assertFalse(account_log.exists())

    def test_linux_service_install_rejects_unsafe_existing_accounts(self):
        home = self.root / "pre-existing-stardust-home"
        home.mkdir()
        cases = (
            ("root-uid", {"STARDUST_USER_UID": "0"}, "user ID must be numeric and nonzero"),
            (
                "root-gid",
                {"STARDUST_GROUP_GID": "0", "STARDUST_USER_GID": "0", "STARDUST_GROUP_IDS": "0"},
                "group ID",
            ),
            (
                "wrong-primary-group",
                {"STARDUST_USER_GID": "701", "STARDUST_GROUP_IDS": "701"},
                "primary group must be stardust",
            ),
            (
                "shared-service-group",
                {"STARDUST_GROUP_MEMBERS": "another-user"},
                "must not include other users",
            ),
            (
                "supplementary-group",
                {"STARDUST_GROUP_IDS": "700 27"},
                "must not have supplementary groups",
            ),
            (
                "login-shell",
                {"STARDUST_USER_SHELL": "/bin/bash"},
                "non-login shell",
            ),
            (
                "existing-home",
                {"STARDUST_USER_HOME": str(home)},
                "home directory must not exist",
            ),
        )
        for name, account, message in cases:
            with self.subTest(account=name):
                result, install_dir, _, _, _ = self._run_linux_service_installer(
                    f"unsafe-{name}", existing_binary=b"pre-existing binary", **account
                )
                existing_binary = install_dir / "stardust"
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertEqual(existing_binary.read_bytes(), b"pre-existing binary")
                commands = (self.root / "systemctl.log").read_text().splitlines()
                self.assertFalse(any(command.startswith(("enable ", "start ")) for command in commands))


if __name__ == "__main__":
    unittest.main()
