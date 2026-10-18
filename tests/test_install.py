import hashlib
import io
import os
import pathlib
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

        fake_tool = self.stubs / "fake-tool"
        fake_tool.write_text(FAKE_TOOL)
        fake_tool.chmod(0o755)
        for name in ("uname", "curl", "sha256sum", "shasum"):
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


if __name__ == "__main__":
    unittest.main()
