#!/usr/bin/env python3
"""Offline unit tests for the CPAP release-pull updater.

Run from the repository root with either of:

    python -m unittest discover -s deployment/cpap-gateway -p 'test_*.py' -v
    python deployment/cpap-gateway/test_cpap_deploy.py -v

The tests never touch the network, systemd, GitHub, or the real server paths.
All HTTP, systemctl, health, and version-probe behaviour is faked with local
temporary directories.
"""

import contextlib
import hashlib
import io
import json
import os
import shutil
import stat
import sys
import tarfile
import tempfile
import unittest
import urllib.parse
from pathlib import Path

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

import cpap_deploy as mod  # noqa: E402


GITHUB_BASE = "https://github.com"
REPO = "SCNU-PIONEER/CLIProxyAPI"


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def regular(name, data, mode=0o755):
    info = tarfile.TarInfo(name)
    info.size = len(data)
    info.mode = mode
    info.type = tarfile.REGTYPE
    return info, data


def symlink(name, target):
    info = tarfile.TarInfo(name)
    info.type = tarfile.SYMTYPE
    info.linkname = target
    info.size = 0
    return info, None


def hardlink(name, target):
    info = tarfile.TarInfo(name)
    info.type = tarfile.LNKTYPE
    info.linkname = target
    info.size = 0
    return info, None


def chardev(name):
    info = tarfile.TarInfo(name)
    info.type = tarfile.CHRTYPE
    info.devmajor = 1
    info.devminor = 3
    return info, None


def make_tarball(members):
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
        for info, data in members:
            archive.addfile(info, io.BytesIO(data) if data is not None else None)
    return buffer.getvalue()


def checksums_text(entries):
    return "".join("%s  %s\n" % (digest, name) for name, digest in sorted(entries.items()))


def release_json(tag, **overrides):
    """A GitHub releases API object for a published stable release."""
    payload = {
        "tag_name": tag,
        "name": "release " + tag,
        "draft": False,
        "prerelease": False,
        "published_at": "2026-01-01T00:00:00Z",
        "assets": [],
    }
    payload.update(overrides)
    return payload


class FakeHttp:
    def __init__(self):
        self.releases = []
        self.release_details = {}
        self.checksums = {}
        self.assets = {}
        self.fail_get = {}
        self.fail_release_list = None
        self.fail_release_detail = {}
        self.fail_download = {}
        self.get_text_calls = []
        self.download_calls = []

    def get_text(self, url, max_bytes):
        self.get_text_calls.append(url)
        if url in self.fail_get:
            raise self.fail_get[url]
        if "/releases?per_page=" in url:
            if self.fail_release_list is not None:
                raise self.fail_release_list
            return json.dumps(self.releases)
        if "/releases/tags/" in url:
            tag = urllib.parse.unquote(url.rsplit("/", 1)[-1])
            if tag in self.fail_release_detail:
                raise self.fail_release_detail[tag]
            payload = self.release_details.get(tag)
            if payload is None:
                raise mod.NotFound("no release for %s" % tag, status=404)
            return json.dumps(payload)
        tag = _checksum_tag(url)
        if tag is not None:
            text = self.checksums.get(tag)
            if text is None:
                raise mod.NotFound("no checksums for %s" % tag, status=404)
            return text
        raise mod.NotFound(url, status=404)

    def download(self, url, destination, max_bytes):
        self.download_calls.append(url)
        if url in self.fail_download:
            raise self.fail_download[url]
        data = self.assets.get(url)
        if data is None:
            raise mod.NotFound("no asset for %s" % url, status=404)
        if len(data) > max_bytes:
            raise mod.FetchError("asset too large in fake")
        destination = Path(destination)
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(data)
        return sha256(data), len(data)


def _checksum_tag(url):
    marker = "/releases/download/"
    if marker not in url or not url.endswith("/checksums.txt"):
        return None
    return url.split(marker, 1)[1].split("/", 1)[0]


class FakeSystemd:
    def __init__(self, exec_start="", working_directory=""):
        self.active = True
        self.enabled = True
        self.restarts = 0
        self.fail_restarts = 0
        self.exec_start_value = exec_start
        self.working_directory_value = working_directory
        self.main_pid_value = 4242

    def is_active(self):
        return self.active

    def is_enabled(self):
        return self.enabled

    def restart(self):
        self.restarts += 1
        if self.fail_restarts > 0:
            self.fail_restarts -= 1
            raise mod.DeployError("fake systemctl restart failed")
        self.active = True

    def exec_start(self):
        return self.exec_start_value

    def working_directory(self):
        return self.working_directory_value

    def main_pid(self):
        return self.main_pid_value


class FakeHealth:
    def __init__(self, results):
        self.results = list(results)
        self.calls = 0

    def __call__(self):
        self.calls += 1
        if not self.results:
            return False
        return bool(self.results.pop(0))


class DeployTestBase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        self.app = self.root / "app"
        self.app.mkdir()
        self.binary = self.app / "cli-proxy-api"
        self.binary.write_bytes(b"old-binary")
        os.chmod(self.binary, 0o755)
        self.config_file = self.app / "config.yaml"
        self.config_file.write_text("port: 8317\n", encoding="utf-8")
        self.auth = self.root / "auth"
        self.auth.mkdir()
        self.state = self.root / "state"
        self.http = FakeHttp()
        self.systemd = FakeSystemd(exec_start=str(self.binary), working_directory=str(self.app))
        self.log = mod.Log(echo=False)
        self.cfg = mod.Config(
            binary=self.binary,
            app_dir=self.app,
            config_file=self.config_file,
            auth_dir=self.auth,
            unit="cliproxyapi.service",
            health_url="http://127.0.0.1:8317/healthz",
            state_dir=self.state,
            repo=REPO,
            github_base=GITHUB_BASE,
            arch="amd64",
            http=self.http,
            systemd=self.systemd,
            sleep=lambda seconds: None,
            probe=self.probe,
            process_probe=self._process_probe,
            process_attempts=2,
            process_interval=0,
            health_probe=FakeHealth([True]),
            log=self.log,
            allow_non_root=True,
            health_attempts=2,
            health_interval=0,
        )
        self.set_feed()

    def tearDown(self):
        self.log.close()
        self._tmp.cleanup()
        shutil.rmtree(str(self.root), ignore_errors=True)

    # -- helpers ---------------------------------------------------------
    def probe(self, path):
        data = Path(path).read_bytes()
        mapping = {
            b"old-binary": "8.0.38",
            b"new-binary": "8.0.39",
            b"newer-binary": "8.0.40",
            b"wrong-version": "8.0.38",
        }
        if data not in mapping:
            raise mod.DeployError("fake probe does not know %r" % data)
        return mapping[data]

    def _process_probe(self, pid):
        return str(self.binary), sha256(self.binary.read_bytes())

    def set_feed(self, *tags, **overrides):
        """Publish a GitHub releases API list; newest first, as GitHub returns it."""
        self.http.releases = []
        self.http.release_details = {}
        for tag in tags:
            payload = release_json(tag, **overrides)
            self.http.releases.append(payload)
            self.http.release_details[tag] = payload

    def set_health(self, *results):
        self.cfg.health_probe = FakeHealth(list(results))

    def publish(self, tag, binary=b"new-binary", checksums=True, asset=True, digest=None):
        name = mod.asset_name(tag, "amd64")
        data = make_tarball([regular("cli-proxy-api", binary), regular("LICENSE", b"license")])
        if asset:
            self.http.assets[mod.release_url(self.cfg, tag, name)] = data
        if checksums:
            self.http.checksums[tag] = checksums_text({name: digest or sha256(data)})
        return name, data, sha256(data)

    def write_state(self, tag, archive_sha=None, asset=None, source="release", binary_version=None):
        payload = {
            "schema": 1,
            "tag": tag,
            "asset": asset if asset is not None else mod.asset_name(tag, "amd64"),
            "sha256": archive_sha,
            "binary_sha256": sha256(self.binary.read_bytes()),
            "binary_version": binary_version or "v8.0.38",
            "previous_tag": None,
            "deployed_at": "2026-01-01T00:00:00Z",
            "source": source,
        }
        mod.atomic_write_json(self.cfg.deployed_path, payload)
        return payload

    def write_journal(
        self,
        tag="v8.0.39",
        new_binary=b"new-binary",
        backup_bytes=b"old-binary",
        phase="swapped",
        previous_state=None,
    ):
        backup = self.app / ("cli-proxy-api.bak." + tag)
        backup.write_bytes(backup_bytes)
        journal = {
            "schema": 1,
            "phase": phase,
            "tag": tag,
            "previous_tag": "v8.0.38",
            "asset": mod.asset_name(tag, "amd64"),
            "archive_sha256": sha256(b"archive-bytes"),
            "new_binary_sha256": sha256(new_binary),
            "binary_version": tag,
            "backup_path": str(backup),
            "backup_sha256": sha256(backup_bytes),
            "live_path": str(self.binary),
            "staged_path": str(self.app / ".cli-proxy-api.new.123"),
            "config_snapshot": None,
            "previous_state": previous_state,
            "mode": 0o755,
            "started_at": "2026-01-01T00:00:00Z",
            "pid": 123,
        }
        mod.atomic_write_json(self.cfg.journal_path, journal)
        return journal

    def run_deploy(self, **kwargs):
        return mod.run_deploy(self.cfg, **kwargs)

    def read_state(self):
        return json.loads(self.cfg.deployed_path.read_text(encoding="utf-8"))

    def failure_record(self, tag="v8.0.39"):
        payload = json.loads(self.cfg.failure_path.read_text(encoding="utf-8"))
        return payload["failures"][tag]


class VersionAndAssetTests(unittest.TestCase):
    def test_parse_stable_tag(self):
        self.assertEqual(mod.parse_stable_tag("v8.0.39").tag, "v8.0.39")
        self.assertEqual(mod.parse_stable_tag("v0.0.0").version, "0.0.0")
        for bad in ("8.0.39", "v8.0", "v8.0.39-rc1", "v08.0.39", "nightly", "", None):
            self.assertIsNone(mod.parse_stable_tag(bad), bad)

    def test_version_ordering(self):
        self.assertGreater(mod.parse_stable_tag("v8.0.10"), mod.parse_stable_tag("v8.0.9"))
        self.assertGreater(mod.parse_stable_tag("v9.0.0"), mod.parse_stable_tag("v8.99.99"))

    def test_normalize_version_text(self):
        self.assertEqual(mod.normalize_version_text("v8.0.39"), "v8.0.39")
        self.assertEqual(mod.normalize_version_text("8.0.39"), "v8.0.39")
        self.assertIsNone(mod.normalize_version_text("dev"))
        self.assertIsNone(mod.normalize_version_text("8.0.39-custom"))

    def test_asset_names(self):
        self.assertEqual(mod.asset_name("v8.0.39", "amd64"), "CLIProxyAPI_8.0.39_linux_amd64.tar.gz")
        self.assertEqual(
            mod.asset_name("v8.0.39", "aarch64"), "CLIProxyAPI_8.0.39_linux_aarch64.tar.gz"
        )
        self.assertNotIn("_no-plugin", mod.asset_name("v8.0.39", "amd64"))
        with self.assertRaises(mod.ConfigError):
            mod.asset_name("v8.0.39", "riscv64")

    def test_detect_arch_override_in_config(self):
        cfg = mod.Config(
            binary="/tmp/x",
            app_dir="/tmp",
            config_file="/tmp/config.yaml",
            auth_dir="/tmp/auth",
            unit="u.service",
            health_url="http://127.0.0.1:1/healthz",
            state_dir="/tmp/state",
            arch="aarch64",
            log=mod.Log(echo=False),
        )
        self.assertEqual(cfg.arch, "aarch64")


class ChecksumsTests(unittest.TestCase):
    def test_parse_ok(self):
        digest = "a" * 64
        text = "%s  CLIProxyAPI_8.0.39_linux_amd64.tar.gz\n%s *other.zip\n" % (digest, "b" * 64)
        entries = mod.parse_checksums(text)
        self.assertEqual(entries["CLIProxyAPI_8.0.39_linux_amd64.tar.gz"], digest)
        self.assertEqual(entries["other.zip"], "b" * 64)

    def test_rejects_malformed(self):
        cases = [
            "not-a-digest  file.tar.gz\n",
            "%s  file.tar.gz\n%s  file.tar.gz\n" % ("a" * 64, "b" * 64),
            "%s  one\n%s  two\n" % ("a" * 64, "a" * 64),
            "%s  ../evil\n" % ("a" * 64,),
            "\n\n",
        ]
        for text in cases:
            with self.assertRaises(mod.ChecksumError, msg=text):
                mod.parse_checksums(text)


class TarSafetyTests(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        self.archive = self.root / "asset.tar.gz"
        self.destination = self.root / "cli-proxy-api"

    def tearDown(self):
        self._tmp.cleanup()

    def _write(self, members):
        self.archive.write_bytes(make_tarball(members))

    def test_extracts_only_binary(self):
        self._write([regular("cli-proxy-api", b"payload"), regular("README.md", b"readme")])
        mod.extract_binary(self.archive, self.destination)
        self.assertEqual(self.destination.read_bytes(), b"payload")
        self.assertFalse((self.root / "README.md").exists())

    def test_rejects_links(self):
        for member in (symlink("cli-proxy-api", "/bin/sh"), hardlink("cli-proxy-api", "other")):
            self._write([member])
            with self.assertRaises(mod.DeployError):
                mod.extract_binary(self.archive, self.destination)

    def test_rejects_path_traversal_and_absolute(self):
        for name in ("../cli-proxy-api", "/cli-proxy-api", "dir/../cli-proxy-api", "a//cli-proxy-api"):
            self._write([regular(name, b"payload")])
            with self.assertRaises(mod.DeployError, msg=name):
                mod.extract_binary(self.archive, self.destination)

    def test_rejects_backslash_and_missing_binary(self):
        self._write([regular("dir\\cli-proxy-api", b"payload")])
        with self.assertRaises(mod.DeployError):
            mod.extract_binary(self.archive, self.destination)
        self._write([regular("LICENSE", b"license")])
        with self.assertRaises(mod.DeployError):
            mod.extract_binary(self.archive, self.destination)

    def test_rejects_duplicate_and_device(self):
        self._write([regular("cli-proxy-api", b"one"), regular("cli-proxy-api", b"two")])
        with self.assertRaises(mod.DeployError):
            mod.extract_binary(self.archive, self.destination)
        self._write([chardev("cli-proxy-api")])
        with self.assertRaises(mod.DeployError):
            mod.extract_binary(self.archive, self.destination)

    def test_rejects_garbage(self):
        self.archive.write_bytes(b"not a tarball")
        with self.assertRaises(mod.DeployError):
            mod.extract_binary(self.archive, self.destination)


class ReleaseApiTests(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        self.http = FakeHttp()
        self.cfg = mod.Config(
            binary=self.root / "binary",
            app_dir=self.root,
            config_file=self.root / "config.yaml",
            auth_dir=self.root / "auth",
            unit="u.service",
            health_url="http://127.0.0.1:1/healthz",
            state_dir=self.root / "state",
            repo=REPO,
            github_base=GITHUB_BASE,
            arch="amd64",
            http=self.http,
            systemd=FakeSystemd(),
            sleep=lambda seconds: None,
            process_probe=lambda pid: ("", ""),
            log=mod.Log(echo=False),
        )
        self.log = self.cfg.log

    def tearDown(self):
        self.log.close()
        self._tmp.cleanup()

    def _publish_ready(self, tag):
        data = make_tarball([regular("cli-proxy-api", b"new-binary")])
        name = mod.asset_name(tag, "amd64")
        self.http.checksums[tag] = checksums_text({name: sha256(data)})
        self.http.assets[mod.release_url(self.cfg, tag, name)] = data

    def test_parse_list_keeps_only_published_stable_releases(self):
        payload = [
            release_json("v8.9.0", prerelease=True),
            release_json("v9.0.0", draft=True),
            release_json("v8.1.0", published_at=None),
            release_json("v8.0.40", name="not a tag"),
            release_json("v8.0.39"),
            release_json("v8.0.38-rc1"),
        ]
        versions = mod.parse_release_list(json.dumps(payload))
        self.assertEqual([v.tag for v in versions], ["v8.0.40", "v8.0.39"])

    def test_tag_name_is_authoritative_not_release_title(self):
        versions = mod.parse_release_list(
            json.dumps([release_json("v8.0.39", name="v9.9.9")])
        )
        self.assertEqual([v.tag for v in versions], ["v8.0.39"])

    def test_empty_and_invalid_payloads(self):
        self.assertEqual(mod.parse_release_list("[]"), [])
        with self.assertRaises(mod.FetchError):
            mod.parse_release_list("not-json")
        with self.assertRaises(mod.FetchError):
            mod.parse_release_list("{}")

    def test_latest_skips_prerelease_with_plain_semver_tag(self):
        self._publish_ready("v8.9.0")
        self._publish_ready("v8.0.39")
        self.http.releases = [
            release_json("v8.9.0", prerelease=True),
            release_json("v8.0.39"),
        ]
        self.http.release_details = {
            "v8.9.0": release_json("v8.9.0", prerelease=True),
            "v8.0.39": release_json("v8.0.39"),
        }
        resolution = mod.choose_resolution(self.cfg, None, None, False)
        self.assertEqual(resolution.version.tag, "v8.0.39")

    def test_latest_skips_draft_and_missing_published_at(self):
        self._publish_ready("v8.0.39")
        self.http.releases = [
            release_json("v8.0.41", draft=True),
            release_json("v8.0.40", published_at=None),
            release_json("v8.0.39"),
        ]
        resolution = mod.choose_resolution(self.cfg, None, None, False)
        self.assertEqual(resolution.version.tag, "v8.0.39")

    def test_explicit_tag_requires_published_release_metadata(self):
        self._publish_ready("v8.0.39")
        self.http.release_details["v8.0.39"] = release_json("v8.0.39", prerelease=True)
        with self.assertRaises(mod.PolicyError):
            mod.choose_resolution(self.cfg, None, "v8.0.39", False)

    def test_explicit_tag_without_release_object_fails(self):
        self._publish_ready("v8.0.39")
        with self.assertRaises(mod.ConfigError):
            mod.choose_resolution(self.cfg, None, "v8.0.39", False)

    def test_rate_limit_is_reported_without_changes(self):
        self.http.fail_release_list = mod.FetchError("HTTP 403", status=403)
        with self.assertRaises(mod.FetchError) as caught:
            mod.choose_resolution(self.cfg, None, None, False)
        self.assertIn("rate limit", str(caught.exception))
        self.assertEqual(self.http.download_calls, [])
        self.assertFalse(self.cfg.state_dir.exists())

    def test_major_upgrade_requires_allow_major(self):
        self._publish_ready("v9.0.0")
        self.http.releases = [release_json("v9.0.0")]
        self.http.release_details = {"v9.0.0": release_json("v9.0.0")}
        current = mod.parse_stable_tag("v8.0.39")
        self.assertIsNone(mod.choose_resolution(self.cfg, current, None, False))
        resolution = mod.choose_resolution(self.cfg, current, None, False, allow_major=True)
        self.assertEqual(resolution.version.tag, "v9.0.0")
        with self.assertRaises(mod.PolicyError):
            mod.choose_resolution(self.cfg, current, "v9.0.0", False)
        explicit = mod.choose_resolution(self.cfg, current, "v9.0.0", False, allow_major=True)
        self.assertEqual(explicit.version.tag, "v9.0.0")


class DeploySuccessTests(DeployTestBase):
    def test_latest_success_replaces_binary_and_records_state(self):
        name, data, digest = self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        config_before = self.config_file.read_bytes()

        code = self.run_deploy()
        self.assertEqual(code, 0)
        self.assertEqual(self.binary.read_bytes(), b"new-binary")
        self.assertTrue((self.app / "cli-proxy-api.bak.v8.0.39").is_file())
        self.assertEqual((self.app / "cli-proxy-api.bak.v8.0.39").read_bytes(), b"old-binary")
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertEqual(self.systemd.restarts, 1)
        state = self.read_state()
        self.assertEqual(state["tag"], "v8.0.39")
        self.assertEqual(state["previous_tag"], "v8.0.38")
        self.assertEqual(state["asset"], name)
        self.assertEqual(state["sha256"], digest)
        self.assertEqual(state["source"], "release")
        self.assertEqual(self.config_file.read_bytes(), config_before)
        snapshot = Path(state["config_snapshot"])
        self.assertTrue(snapshot.is_file())
        self.assertEqual(snapshot.read_bytes(), config_before)
        if os.name == "posix":
            self.assertEqual(stat.S_IMODE(os.stat(snapshot).st_mode), 0o600)
        self.assertIn("deployed", self.cfg.log_path.read_text(encoding="utf-8").lower())

    def test_explicit_tag_deploy(self):
        self.set_feed("v8.0.39")
        self.publish("v8.0.39")
        code = self.run_deploy(explicit_tag="8.0.39")
        self.assertEqual(code, 0)
        self.assertEqual(self.read_state()["tag"], "v8.0.39")

    def test_explicit_tag_not_ready(self):
        self.set_feed("v8.0.39")
        with self.assertRaises(mod.ConfigError):
            self.run_deploy(explicit_tag="v8.0.39")

    def test_downgrade_refused(self):
        self.write_state("v8.0.39", sha256(b"archive"))
        with self.assertRaises(mod.PolicyError):
            self.run_deploy(explicit_tag="v8.0.38")

    def test_partial_release_falls_back_to_older_ready_release(self):
        # v8.0.41 has an asset but its checksums entry is missing.
        self.publish("v8.0.41", binary=b"newer-binary", checksums=False)
        name = mod.asset_name("v8.0.41", "amd64")
        self.http.checksums["v8.0.41"] = checksums_text({"other-asset.tar.gz": "c" * 64})
        # v8.0.40 has no checksums.txt at all.
        self.publish("v8.0.40", binary=b"newer-binary", checksums=False)
        self.publish("v8.0.39", binary=b"new-binary")
        self.set_feed("v8.0.41", "v8.0.40", "v8.0.39")
        with contextlib.redirect_stdout(io.StringIO()):
            code = self.run_deploy()
        self.assertEqual(code, 0)
        self.assertEqual(self.read_state()["tag"], "v8.0.39")

    def test_newer_release_not_ready_is_noop(self):
        self.publish("v8.0.40", binary=b"newer-binary", checksums=False)
        self.set_feed("v8.0.40")
        code = self.run_deploy()
        self.assertEqual(code, 0)
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertEqual(self.systemd.restarts, 0)
        self.assertFalse(self.cfg.deployed_path.exists())

    def test_same_version_is_noop_when_digest_matches(self):
        name, data, digest = self.publish("v8.0.39")
        self.write_state("v8.0.39", digest, asset=name)
        self.set_feed("v8.0.39")
        self.assertEqual(self.run_deploy(), 0)
        self.assertEqual(self.systemd.restarts, 0)
        self.assertEqual(self.http.download_calls, [])

    def test_same_version_changed_digest_fails_closed(self):
        name, data, digest = self.publish("v8.0.39")
        self.write_state("v8.0.39", sha256(b"different-archive"), asset=name)
        self.set_feed("v8.0.39")
        self.assertEqual(self.run_deploy(), 5)
        self.assertEqual(self.systemd.restarts, 0)
        self.assertEqual(self.http.download_calls, [])
        self.assertEqual(self.binary.read_bytes(), b"old-binary")

    def test_same_version_missing_asset_entry_fails_closed(self):
        self.publish("v8.0.39")
        self.write_state("v8.0.39", sha256(b"archive"))
        self.http.checksums["v8.0.39"] = checksums_text({"other.tar.gz": "c" * 64})
        self.set_feed("v8.0.39")
        self.assertEqual(self.run_deploy(), 5)

    def test_same_version_checksums_404_fails_closed(self):
        self.write_state("v8.0.39", sha256(b"archive"))
        self.http.checksums["v8.0.39"] = None
        self.set_feed("v8.0.39")
        self.assertEqual(self.run_deploy(), 5)

    def test_network_error_during_integrity_check_is_warning(self):
        self.write_state("v8.0.39", sha256(b"archive"))
        self.http.fail_get[mod.checksums_url(self.cfg, "v8.0.39")] = mod.FetchError("offline")
        self.set_feed("v8.0.39")
        self.assertEqual(self.run_deploy(), 0)

    def test_dry_run_writes_nothing_and_does_not_download(self):
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            code = self.run_deploy(dry_run=True)
        self.assertEqual(code, 0)
        self.assertIn("v8.0.39", output.getvalue())
        self.assertFalse(self.state.exists())
        self.assertEqual(self.http.download_calls, [])
        self.assertEqual(self.binary.read_bytes(), b"old-binary")

    def test_failed_release_is_skipped_until_retry(self):
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        mod.atomic_write_json(
            self.cfg.failure_path,
            {"tag": "v8.0.39", "reason": "boom", "rolled_back": True, "at": "2026-01-01T00:00:00Z"},
        )
        self.assertEqual(self.run_deploy(), 0)
        self.assertEqual(self.http.download_calls, [])
        self.assertEqual(self.systemd.restarts, 0)
        self.assertEqual(self.run_deploy(retry=True), 0)
        self.assertEqual(self.read_state()["tag"], "v8.0.39")

    def test_live_binary_drift_fails_closed(self):
        self.write_state("v8.0.39")
        self.binary.write_bytes(b"new-binary")
        self.assertEqual(self.run_deploy(), 5)
        self.assertEqual(self.systemd.restarts, 0)
        self.assertFalse(self.cfg.journal_path.exists())

    def test_live_version_drift_fails_closed_without_recorded_sha(self):
        payload = self.write_state("v8.0.39", binary_version="v8.0.39")
        payload.pop("binary_sha256")
        mod.atomic_write_json(self.cfg.deployed_path, payload)
        self.assertEqual(self.run_deploy(), 5)
        self.assertEqual(self.systemd.restarts, 0)

    def test_baseline_state_without_digest_skips_integrity_check(self):
        self.write_state("v8.0.39", None, source="baseline")
        self.set_feed("v8.0.39")
        self.assertEqual(self.run_deploy(), 0)

    def test_keep_backups_prunes_only_our_prefix(self):
        for index in range(4):
            path = self.app / ("cli-proxy-api.bak.v7.0.%d" % index)
            path.write_bytes(b"old")
            os.utime(path, (1000 + index, 1000 + index))
        manual = self.app / "manual-backup.tar.gz"
        manual.write_bytes(b"keep me")
        self.cfg.keep_backups = 2
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        self.assertEqual(self.run_deploy(), 0)
        backups = sorted(name for name in os.listdir(str(self.app)) if name.startswith("cli-proxy-api.bak."))
        self.assertEqual(len(backups), 2)
        self.assertIn("cli-proxy-api.bak.v8.0.39", backups)
        self.assertTrue(manual.is_file())

    def test_dry_run_reports_pending_failed_journal(self):
        self.write_journal(phase="failed")
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            code = self.run_deploy(dry_run=True)
        self.assertEqual(code, 5)
        self.assertIn("manual intervention", output.getvalue())


class DeployFailureTests(DeployTestBase):
    def test_checksum_mismatch_marks_failure_and_keeps_binary(self):
        self.publish("v8.0.39", digest="d" * 64)
        self.set_feed("v8.0.39")
        self.assertEqual(self.run_deploy(), 1)
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertEqual(self.systemd.restarts, 0)
        failure = self.failure_record()
        self.assertEqual(failure["tag"], "v8.0.39")
        self.assertFalse(failure["rolled_back"])
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertFalse(self.cfg.deployed_path.exists())

    def test_staged_version_mismatch_marks_failure(self):
        self.publish("v8.0.39", binary=b"wrong-version")
        self.set_feed("v8.0.39")
        self.assertEqual(self.run_deploy(), 1)
        failure = self.failure_record()
        self.assertIn("reports version", failure["reason"])
        self.assertEqual(self.binary.read_bytes(), b"old-binary")

    def test_asset_404_is_retryable_and_not_marked(self):
        name = mod.asset_name("v8.0.39", "amd64")
        data = make_tarball([regular("cli-proxy-api", b"new-binary")])
        self.http.checksums["v8.0.39"] = checksums_text({name: sha256(data)})
        self.set_feed("v8.0.39")
        self.assertEqual(self.run_deploy(), 1)
        self.assertFalse(self.cfg.failure_path.exists())
        self.assertEqual(self.binary.read_bytes(), b"old-binary")

    def test_health_failure_rolls_back(self):
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        self.set_health(False, False, True)
        self.assertEqual(self.run_deploy(), 1)
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertEqual(self.systemd.restarts, 2)
        failure = self.failure_record()
        self.assertTrue(failure["rolled_back"])
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertFalse(self.cfg.deployed_path.exists())

    def test_restart_failure_rolls_back(self):
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        self.systemd.fail_restarts = 1
        self.set_health(True)
        self.assertEqual(self.run_deploy(), 1)
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertEqual(self.systemd.restarts, 2)
        failure = self.failure_record()
        self.assertTrue(failure["rolled_back"])

    def test_process_identity_mismatch_rolls_back(self):
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        self.set_health(True, True)
        good = self._process_probe
        calls = {"count": 0}

        def stale_first(pid):
            calls["count"] += 1
            if calls["count"] <= 2:
                return str(self.app / "stale-binary"), "0" * 64
            return good(pid)

        self.cfg.process_probe = stale_first
        self.assertEqual(self.run_deploy(), 1)
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        failure = self.failure_record()
        self.assertTrue(failure["rolled_back"])
        self.assertIn("did not settle", failure["reason"])
        self.assertFalse(self.cfg.journal_path.exists())

    def test_working_directory_drift_blocks_before_any_download(self):
        self.write_state("v8.0.38")
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        self.systemd.working_directory_value = str(self.root / "elsewhere")
        with self.assertRaisesRegex(mod.ConfigError, "WorkingDirectory"):
            self.run_deploy()
        self.assertEqual(self.http.download_calls, [])
        self.assertEqual(self.systemd.restarts, 0)
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertEqual(self.read_state()["tag"], "v8.0.38")

    def test_missing_working_directory_blocks_before_any_download(self):
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        self.systemd.working_directory_value = ""
        with self.assertRaisesRegex(mod.ConfigError, "has no WorkingDirectory"):
            self.run_deploy()
        self.assertEqual(self.http.download_calls, [])
        self.assertEqual(self.systemd.restarts, 0)
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertFalse(self.cfg.deployed_path.exists())

    def test_rollback_failure_keeps_journal_for_manual_intervention(self):
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        self.set_health(False, False, False, False)
        self.assertEqual(self.run_deploy(), 5)
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertTrue(self.cfg.journal_path.exists())
        journal = json.loads(self.cfg.journal_path.read_text(encoding="utf-8"))
        self.assertEqual(journal["phase"], "rollback_failed")
        failure = self.failure_record()
        self.assertTrue(failure["rolled_back"])
        self.assertIn("rollback health check failed", failure["reason"])
        restarts = self.systemd.restarts
        # The next timer run must not discard the journal and try the same
        # release again while the service is unhealthy.
        with self.assertRaises(mod.RecoveryError):
            self.run_deploy()
        self.assertTrue(self.cfg.journal_path.exists())
        self.assertEqual(self.systemd.restarts, restarts)


class TransactionFaultTests(DeployTestBase):
    def test_commit_cleanup_error_never_rolls_back(self):
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        original = mod.prune_backups

        def fail(cfg, protect=None):
            raise OSError("simulated disk full during prune")

        mod.prune_backups = fail
        try:
            self.assertEqual(self.run_deploy(), 0)
        finally:
            mod.prune_backups = original
        self.assertEqual(self.binary.read_bytes(), b"new-binary")
        self.assertEqual(self.read_state()["tag"], "v8.0.39")
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertEqual(self.systemd.restarts, 1)

    def test_commit_logging_error_never_rolls_back(self):
        self.write_state("v8.0.38")
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        original_prune = mod.prune_backups
        original_warn = self.cfg.log.warn

        def fail_prune(cfg, protect=None):
            raise OSError("simulated prune failure")

        def fail_warn(message, *args):
            raise OSError("simulated log failure")

        mod.prune_backups = fail_prune
        self.cfg.log.warn = fail_warn
        try:
            self.assertEqual(self.run_deploy(), 0)
        finally:
            mod.prune_backups = original_prune
            self.cfg.log.warn = original_warn
        self.assertEqual(self.binary.read_bytes(), b"new-binary")
        self.assertEqual(self.read_state()["tag"], "v8.0.39")
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertEqual(self.systemd.restarts, 1)

    def test_failed_tag_map_is_bounded(self):
        for index in range(25):
            mod.mark_failure(self.cfg, "v9.0.%d" % index, "simulated failure", False)
        payload = json.loads(self.cfg.failure_path.read_text(encoding="utf-8"))
        self.assertEqual(len(payload["failures"]), mod.MAX_FAILED_TAGS)

    def test_app_dir_fsync_failure_happens_before_journal(self):
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        original = mod.fsync_dir

        def fail(path):
            if os.path.realpath(str(path)) == os.path.realpath(str(self.app)):
                raise OSError("simulated fsync failure")
            return original(path)

        mod.fsync_dir = fail
        try:
            self.assertEqual(self.run_deploy(), 1)
        finally:
            mod.fsync_dir = original
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertEqual(self.systemd.restarts, 0)

    def test_prune_protects_current_backup_even_when_it_is_older(self):
        self.cfg.keep_backups = 2
        current = self.app / "cli-proxy-api.bak.v8.0.39"
        current.write_bytes(b"current")
        os.utime(current, (1000, 1000))
        for name, stamp in (("v8.0.40", 2000), ("v8.0.41", 3000)):
            path = self.app / ("cli-proxy-api.bak." + name)
            path.write_bytes(name.encode())
            os.utime(path, (stamp, stamp))
        mod.prune_backups(self.cfg, protect=str(current))
        backups = sorted(p.name for p in self.app.glob("cli-proxy-api.bak.*"))
        self.assertIn(current.name, backups)
        self.assertEqual(len(backups), 2)
        self.assertIn("cli-proxy-api.bak.v8.0.41", backups)

    def test_corrupt_journal_is_reported_as_unsafe_state(self):
        mod.ensure_dir(self.cfg.journal_path.parent, 0o700)
        self.cfg.journal_path.write_text("{not-json", encoding="utf-8")
        with self.assertRaises(mod.RecoveryError):
            self.run_deploy()
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertEqual(self.systemd.restarts, 0)


class RecoveryTests(DeployTestBase):
    def test_recovery_commits_when_new_binary_is_installed(self):
        journal = self.write_journal()
        self.binary.write_bytes(b"new-binary")
        self.http.checksums["v8.0.39"] = checksums_text(
            {journal["asset"]: journal["archive_sha256"]}
        )
        self.assertEqual(self.run_deploy(), 0)
        self.assertEqual(self.read_state()["tag"], "v8.0.39")
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertEqual(self.systemd.restarts, 1)

    def test_recovery_rolls_back_when_new_binary_is_unhealthy(self):
        self.write_journal()
        self.binary.write_bytes(b"new-binary")
        self.set_health(False, False, True)
        self.assertEqual(self.run_deploy(), 1)
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertFalse(self.cfg.journal_path.exists())
        failure = self.failure_record()
        self.assertTrue(failure["rolled_back"])

    def test_recovery_discards_prepared_journal_without_backup(self):
        journal = self.write_journal(phase="prepared")
        Path(journal["backup_path"]).unlink()
        self.assertEqual(self.run_deploy(), 0)
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertEqual(self.systemd.restarts, 0)

    def test_recovery_rollback_restores_previous_deployed_state(self):
        old_state = self.write_state("v8.0.38", archive_sha=sha256(b"old-archive"))
        self.binary.write_bytes(b"new-binary")
        self.write_state(
            "v8.0.39",
            archive_sha=sha256(b"new-archive"),
            binary_version="v8.0.39",
        )
        self.write_journal(previous_state=old_state)
        self.set_health(False, False, True)
        self.assertEqual(self.run_deploy(), 1)
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        state = self.read_state()
        self.assertEqual(state["tag"], "v8.0.38")
        self.assertEqual(state["binary_sha256"], sha256(b"old-binary"))
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertTrue(self.failure_record()["rolled_back"])

    def test_recovery_finalizes_rolling_back_without_redeploying(self):
        old_state = self.write_state("v8.0.38", archive_sha=sha256(b"old-archive"))
        self.publish("v8.0.39")
        self.set_feed("v8.0.39")
        self.write_journal(phase="rolling_back", previous_state=old_state)
        self.set_health(True)
        self.assertEqual(self.run_deploy(), 1)
        self.assertEqual(self.http.download_calls, [])
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertEqual(self.read_state()["tag"], "v8.0.38")
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertEqual(self.systemd.restarts, 1)
        self.assertTrue(self.failure_record()["rolled_back"])

    def test_failed_tag_remains_blocked_after_older_release_deploys(self):
        self.publish("v8.0.39")
        self.publish("v8.0.40")
        self.set_feed("v8.0.40", "v8.0.39")
        mod.mark_failure(self.cfg, "v8.0.40", "simulated failure", False)
        self.assertEqual(self.run_deploy(), 0)
        self.assertEqual(self.read_state()["tag"], "v8.0.39")
        downloads = len(self.http.download_calls)
        self.assertEqual(self.run_deploy(), 0)
        self.assertEqual(len(self.http.download_calls), downloads)
        self.assertEqual(self.systemd.restarts, 1)
        failure = self.failure_record("v8.0.40")
        self.assertEqual(failure["reason"], "simulated failure")
        self.assertFalse(failure["rolled_back"])

    def test_recovery_discards_when_swap_never_happened(self):
        journal = self.write_journal(phase="prepared")
        staged = Path(journal["staged_path"])
        staged.write_bytes(b"new-binary")
        self.assertEqual(self.run_deploy(), 0)
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertFalse(staged.exists())
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertEqual(self.systemd.restarts, 0)

    def test_recovery_aborts_on_inconsistent_live_binary(self):
        self.write_journal()
        self.binary.write_bytes(b"unexpected-binary")
        with self.assertRaises(mod.RecoveryError):
            self.run_deploy()
        self.assertTrue(self.cfg.journal_path.exists())
        self.assertEqual(self.systemd.restarts, 0)

    def test_recovery_aborts_on_failed_phase(self):
        self.write_journal(phase="failed")
        with self.assertRaises(mod.RecoveryError):
            self.run_deploy()

    def test_abort_journal_removes_it_without_touching_binary(self):
        self.write_journal()
        self.assertEqual(mod.abort_journal(self.cfg), 0)
        self.assertFalse(self.cfg.journal_path.exists())
        self.assertEqual(self.binary.read_bytes(), b"old-binary")
        self.assertEqual(self.systemd.restarts, 0)

    def test_lock_is_exclusive(self):
        if not hasattr(mod, "fcntl") or mod.fcntl is None:
            self.skipTest("flock is unavailable on this platform")
        lock = mod.Lock(self.cfg.lock_path)
        self.assertTrue(lock.acquire())
        try:
            self.assertEqual(self.run_deploy(), 4)
        finally:
            lock.release()


class BaselineTests(DeployTestBase):
    def test_records_probed_version_once(self):
        self.assertEqual(mod.record_baseline(self.cfg, None), 0)
        state = self.read_state()
        self.assertEqual(state["tag"], "v8.0.38")
        self.assertEqual(state["source"], "baseline")
        self.assertIsNone(state["sha256"])
        self.assertEqual(mod.record_baseline(self.cfg, None), 0)

    def test_explicit_version(self):
        self.assertEqual(mod.record_baseline(self.cfg, "8.0.39"), 0)
        self.assertEqual(self.read_state()["tag"], "v8.0.39")

    def test_missing_version_is_not_taken_from_version_file(self):
        (self.app / "version.txt").write_text("v8.0.39\n", encoding="utf-8")

        def broken_probe(path):
            raise mod.DeployError("cannot execute")

        self.cfg.probe = broken_probe
        with self.assertRaises(mod.ConfigError):
            mod.record_baseline(self.cfg, None)
        self.assertFalse(self.cfg.deployed_path.exists())

    def test_invalid_explicit_version(self):
        with self.assertRaises(mod.ConfigError):
            mod.record_baseline(self.cfg, "latest")


class StatusAndCheckTests(DeployTestBase):
    def test_inspect_prints_status_without_writing(self):
        self.publish("v8.0.39")
        self.write_state("v8.0.39", sha256(b"archive"))
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            self.assertEqual(mod.inspect_status(self.cfg), 0)
        text = output.getvalue()
        self.assertIn("CLIProxyAPI release-pull deploy status", text)
        self.assertIn("deployed:       v8.0.39", text)
        self.assertIn("lock:           free", text)

    def test_environment_check_passes_with_fakes(self):
        self.assertEqual(mod.check_environment(self.cfg), 0)

    def test_environment_check_fails_without_binary(self):
        self.binary.unlink()
        self.assertEqual(mod.check_environment(self.cfg), 1)


class CommandLineTests(DeployTestBase):
    def test_mode_conflicts_return_usage_error(self):
        self.assertEqual(mod.main(["--latest", "--tag", "v8.0.39"]), 2)
        self.assertEqual(mod.main(["--retry"]), 2)
        self.assertEqual(mod.main(["--allow-major"]), 2)
        self.assertEqual(mod.main(["--baseline-version", "v8.0.39"]), 2)

    def test_dry_run_can_preview_a_major_upgrade(self):
        parser = mod.build_parser()
        mod.validate_args(parser.parse_args(["--dry-run", "--allow-major"]))
        mod.validate_args(parser.parse_args(["--tag", "v8.0.39", "--dry-run"]))

    def test_tag_cannot_be_combined_with_other_modes(self):
        parser = mod.build_parser()
        for argv in (
            ["--tag", "v8.0.39", "--check"],
            ["--tag", "v8.0.39", "--baseline"],
            ["--tag", "v8.0.39", "--abort-journal"],
        ):
            with self.assertRaises(mod.ConfigError):
                mod.validate_args(parser.parse_args(argv))

    def test_parser_accepts_modes(self):
        parser = mod.build_parser()
        self.assertTrue(parser.parse_args(["--latest"]).latest)
        self.assertEqual(parser.parse_args(["--tag", "v8.0.39"]).tag, "v8.0.39")
        self.assertTrue(parser.parse_args(["--dry-run"]).dry_run)
        self.assertTrue(parser.parse_args(["--check"]).check)
        self.assertTrue(parser.parse_args(["--latest", "--allow-major"]).allow_major)
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                parser.parse_args(["--check", "--dry-run"])

    def test_invalid_port_rejected(self):
        parser = mod.build_parser()
        args = parser.parse_args(["--port", "0"])
        with self.assertRaises(mod.ConfigError):
            mod.config_from_args(args)


class ProbeTests(unittest.TestCase):
    @unittest.skipUnless(os.name == "posix", "shell-script probe test is POSIX only")
    def test_probe_uses_scratch_cwd(self):
        tmp = tempfile.mkdtemp()
        try:
            cwd_file = os.path.join(tmp, "cwd.txt")
            binary = os.path.join(tmp, "cli-proxy-api")
            with open(binary, "w", encoding="utf-8") as handle:
                handle.write(
                    "#!/bin/sh\npwd > %s\necho 'CLIProxyAPI Version: 9.9.9, Commit: abc, BuiltAt: now'\nexit 2\n"
                    % cwd_file
                )
            os.chmod(binary, 0o755)
            version = mod.probe_binary_version(binary)
            self.assertEqual(version, "9.9.9")
            with open(cwd_file, "r", encoding="utf-8") as handle:
                observed_cwd = handle.read().strip()
            self.assertNotEqual(os.path.realpath(observed_cwd), os.path.realpath(tmp))
        finally:
            shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    unittest.main(verbosity=2)
