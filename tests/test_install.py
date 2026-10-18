import hashlib
import io
import os
import pathlib
import shutil
import subprocess
import tarfile
import tempfile
import unittest


REPO_ROOT = pathlib.Path(__file__).resolve().parents[1]
INSTALLER = REPO_ROOT / "install.sh"

FAKE_TOOL = r'''#!/usr/bin/env python3
import hashlib
import os
import pathlib
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
    else:
        name = url.rsplit("/", 1)[-1]
        if os.environ.get("STARDUST_FAIL_DOWNLOAD") == name:
            sys.exit(22)
    source = fixtures / name
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
        bad_checksums = []
        for entry in checksums:
            digest, name = entry.split()
            replacement = ("0" if digest[0] != "0" else "1") + digest[1:]
            bad_checksums.append(f"{replacement}  {name}")
        (self.fixtures / "checksums-bad.txt").write_text("\n".join(bad_checksums) + "\n")
        shutil.copyfile(REPO_ROOT / "deploy/systemd/stardust.service", self.fixtures / "stardust.service")

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
        ):
            (self.stubs / name).symlink_to(fake_tool)

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
                "TMPDIR": str(self.root),
            }
        )
        env.update(overrides)
        return env

    def _run_installer(self, install_dir, *, version=None, env=None):
        command = ["bash", str(INSTALLER), "--no-service", "--install-dir", str(install_dir)]
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

        script = INSTALLER.read_text()
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
                self.assertIn(f"https://github.com/ch55secake/stardust{release_path}{name}", requests)

    def test_checksum_failure_does_not_replace_existing_install(self):
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
