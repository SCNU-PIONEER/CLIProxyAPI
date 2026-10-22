#!/usr/bin/env python3
"""Release-pull updater for a systemd-managed CLIProxyAPI server.

This program is the server side of the CPAP deployment pipeline. A systemd
timer runs it every five minutes; it looks at the public GitHub Releases of
SCNU-PIONEER/CLIProxyAPI, selects the newest stable ``vX.Y.Z`` release whose
Linux archive and matching ``checksums.txt`` entry are both published, verifies
the archive, and atomically replaces only the server binary.

Safety invariants (deliberate, do not weaken):

* Only the live binary is replaced. ``config.yaml``, the OAuth/auth directory,
  and the application log directory are never written. A read-only copy of
  ``config.yaml`` is snapshotted for operator recovery; it is never restored
  automatically and binary rollback does not undo application-side migration.
* A stable release is identified by the exact tag ``vX.Y.Z``. Pre-releases,
  scratch tags, and the portable ``_no-plugin`` builds are not eligible.
* The archive SHA-256 must match the matching entry in the release
  ``checksums.txt``; tar members are validated and only the regular
  ``cli-proxy-api`` file is extracted (no links, no traversal).
* Downgrades are refused. A release that already failed a deployment is not
  retried until an operator passes ``--retry``.
* The binary is backed up next to the live file, a journal is fsynced before
  the atomic replace, and an interrupted transaction is recovered on the next
  run. A failed restart or health check restores the backup automatically.
* No GitHub token, SSH key, or other credential is used or needed.

Exit codes: 0 success/no-op, 1 deploy or network failure, 2 configuration or
usage error, 3 refused by policy (downgrade, major upgrade without
``--allow-major``, previously failed release without ``--retry``), 4 another
run holds the lock, 5 unsafe state that needs an operator (journal mismatch,
live binary drift, mutated same-version release).
"""

import argparse
import dataclasses
import datetime
import hashlib
import hmac
import json
import os
import platform
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

try:  # pragma: no cover - the fallback exists for local (non-Linux) test runs
    import fcntl
except ImportError:  # pragma: no cover
    fcntl = None


DEFAULT_REPO = "SCNU-PIONEER/CLIProxyAPI"
DEFAULT_GITHUB_BASE = "https://github.com"
DEFAULT_BINARY = "/root/cliproxyapi/cli-proxy-api"
DEFAULT_APP_DIR = "/root/cliproxyapi"
DEFAULT_AUTH_DIR = "/root/.cli-proxy-api"
DEFAULT_UNIT = "cliproxyapi.service"
DEFAULT_PORT = 8317
DEFAULT_STATE_DIR = "/root/cliproxyapi.deploy"
DEFAULT_KEEP_BACKUPS = 5

BINARY_NAME = "cli-proxy-api"
BACKUP_PREFIX = BINARY_NAME + ".bak."
STAGED_PREFIX = "." + BINARY_NAME + ".new."
ROLLBACK_PREFIX = "." + BINARY_NAME + ".rollback."

STABLE_TAG_RE = re.compile(r"^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$")
REPO_RE = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
GITHUB_HOST = "github.com"
GITHUB_API_BASE = "https://api.github.com"
VERSION_LINE_RE = re.compile(r"CLIProxyAPI Version:\s*([^,\s]+)")
CHECKSUM_RE = re.compile(r"^([0-9a-fA-F]{64})$")

MAX_ASSET_BYTES = 512 * 1024 * 1024
MAX_BINARY_BYTES = 512 * 1024 * 1024
MAX_CHECKSUM_BYTES = 2 * 1024 * 1024
MAX_API_BYTES = 4 * 1024 * 1024
MAX_PROBED_RELEASES = 5
MAX_FAILED_TAGS = 20
LOG_ROTATE_BYTES = 2 * 1024 * 1024
USER_AGENT = "cpap-release-pull-deploy/1"

STATE_SCHEMA = 1


# ---------------------------------------------------------------------------
# Errors
# ---------------------------------------------------------------------------


class ConfigError(Exception):
    """Invalid configuration, missing prerequisite, or unsupported platform."""


class DeployError(Exception):
    """A deployment step failed.

    ``mark_failed`` means the release artifact or the running service was
    already affected, so the tag is remembered and not retried automatically.
    """

    def __init__(self, message, mark_failed=False):
        super().__init__(message)
        self.mark_failed = mark_failed


class PolicyError(Exception):
    """A deliberate policy refusal (downgrade, failed release without --retry)."""


class RecoveryError(Exception):
    """A previous transaction cannot be recovered safely automatically."""


class FetchError(Exception):
    """A remote request failed. ``status`` is set for HTTP errors."""

    def __init__(self, message, status=None):
        super().__init__(message)
        self.status = status


class NotFound(FetchError):
    """The remote resource does not exist (HTTP 404)."""


class ChecksumError(Exception):
    """The release ``checksums.txt`` is malformed or inconsistent."""


# ---------------------------------------------------------------------------
# Small utilities
# ---------------------------------------------------------------------------


def utcnow():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def is_root():
    return hasattr(os, "geteuid") and os.geteuid() == 0


def fsync_dir(path):
    """Best-effort directory fsync (POSIX only; Windows has no equivalent)."""
    if os.name != "posix":
        return
    try:
        fd = os.open(str(path), os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    except OSError:
        pass
    finally:
        os.close(fd)


def sha256_file(path):
    digest = hashlib.sha256()
    with open(str(path), "rb") as handle:
        while True:
            chunk = handle.read(1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def read_json(path):
    try:
        with open(str(path), "r", encoding="utf-8") as handle:
            return json.load(handle)
    except FileNotFoundError:
        return None


def atomic_write_json(path, payload, mode=0o600):
    """Write JSON atomically: temp file, fsync, rename, fsync directory."""
    path = str(path)
    parent = os.path.dirname(path)
    if parent and not os.path.isdir(parent):
        ensure_dir(parent, 0o700)
    tmp = "%s.tmp.%d" % (path, os.getpid())
    data = (json.dumps(payload, indent=2, sort_keys=True) + "\n").encode("utf-8")
    try:
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, mode)
        try:
            os.write(fd, data)
            os.fsync(fd)
        finally:
            os.close(fd)
        os.replace(tmp, path)
        fsync_dir(os.path.dirname(path) or ".")
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def ensure_dir(path, mode=0o700):
    path = str(path)
    if not os.path.isdir(path):
        os.makedirs(path, mode=mode, exist_ok=True)


def remove_file(path):
    try:
        os.unlink(str(path))
    except FileNotFoundError:
        pass


def normalize_version_text(text):
    """Return a canonical ``vX.Y.Z`` tag from user input or probe output."""
    value = (text or "").strip()
    if value.startswith("v"):
        value = value[1:]
    match = re.fullmatch(r"(\d+)\.(\d+)\.(\d+)", value)
    if not match:
        return None
    return "v%d.%d.%d" % (int(match.group(1)), int(match.group(2)), int(match.group(3)))


@dataclasses.dataclass(frozen=True, order=True)
class Version:
    major: int
    minor: int
    patch: int
    tag: str = dataclasses.field(compare=False)

    @property
    def version(self):
        """The release version without the leading ``v`` (asset name part)."""
        return self.tag[1:]


def parse_stable_tag(tag):
    match = STABLE_TAG_RE.match((tag or "").strip())
    if not match:
        return None
    major, minor, patch = (int(match.group(1)), int(match.group(2)), int(match.group(3)))
    return Version(major, minor, patch, "v%d.%d.%d" % (major, minor, patch))


def detect_arch():
    machine = platform.machine().lower()
    mapping = {
        "x86_64": "amd64",
        "amd64": "amd64",
        "aarch64": "aarch64",
        "arm64": "aarch64",
    }
    arch = mapping.get(machine)
    if not arch:
        raise ConfigError(
            "unsupported CPU architecture %r; only linux amd64 and aarch64 are supported" % machine
        )
    return arch


def asset_name(tag, arch):
    """Return the plugin-capable Linux asset name for a tag and architecture.

    The release workflow maps ``arm64`` to the ``aarch64`` asset suffix. The
    default glibc build is the plugin-capable one; the ``_no-plugin`` archive
    is intentionally never selected.
    """
    version = parse_stable_tag(tag)
    if version is None:
        raise ConfigError("not a stable release tag: %r" % (tag,))
    if arch not in ("amd64", "aarch64"):
        raise ConfigError("unsupported architecture %r" % (arch,))
    name = "CLIProxyAPI_%s_linux_%s.tar.gz" % (version.version, arch)
    if "_no-plugin" in name:  # defensive: never deploy the portable build
        raise ConfigError("refusing to select a no-plugin asset: %s" % name)
    return name


def release_url(cfg, tag, name):
    return "%s/%s/releases/download/%s/%s" % (
        cfg.github_base.rstrip("/"),
        cfg.repo,
        tag,
        name,
    )


def checksums_url(cfg, tag):
    return release_url(cfg, tag, "checksums.txt")


# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------


class Log:
    def __init__(self, echo=True, path=None):
        self.echo = echo
        self.path = str(path) if path else None
        self._handle = None

    def _ensure_handle(self):
        if self._handle is not None or not self.path:
            return
        directory = os.path.dirname(self.path)
        if directory:
            ensure_dir(directory)
        try:
            if os.path.getsize(self.path) > LOG_ROTATE_BYTES:
                os.replace(self.path, self.path + ".1")
        except OSError:
            pass
        self._handle = open(self.path, "a", encoding="utf-8")

    def _write(self, level, message):
        line = "%s [%s] %s" % (utcnow(), level, message)
        if self.echo:
            try:
                print(line, flush=True)
            except OSError:
                pass
        if self.path:
            try:
                self._ensure_handle()
                self._handle.write(line + "\n")
                self._handle.flush()
            except OSError:
                # Logging must never turn a successful transaction into a
                # failed one. Fall back to no file log for the rest of the run.
                try:
                    if self._handle is not None:
                        self._handle.close()
                except OSError:
                    pass
                self._handle = None
                self.path = None
                try:
                    sys.stderr.write(
                        "cpap-deploy: WARN log file became unwritable; continuing without it\n"
                    )
                except OSError:
                    pass

    def info(self, message, *args):
        self._write("INFO", message % args if args else message)

    def warn(self, message, *args):
        self._write("WARN", message % args if args else message)

    def error(self, message, *args):
        self._write("ERROR", message % args if args else message)

    def step(self, message, *args):
        self._write("STEP", message % args if args else message)

    def close(self):
        if self._handle is not None:
            self._handle.close()
            self._handle = None


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------


@dataclasses.dataclass
class Config:
    binary: object
    app_dir: object
    config_file: object
    auth_dir: object
    unit: str
    health_url: str
    state_dir: object
    repo: str = DEFAULT_REPO
    github_base: str = DEFAULT_GITHUB_BASE
    arch: str = ""
    keep_backups: int = DEFAULT_KEEP_BACKUPS
    health_attempts: int = 20
    health_interval: float = 2.0
    http_timeout: float = 20.0
    allow_non_root: bool = False
    echo: bool = True
    process_probe: object = None
    process_attempts: int = 20
    process_interval: float = 0.25
    http: object = None
    systemd: object = None
    sleep: object = None
    probe: object = None
    health_probe: object = None
    log: object = None

    def __post_init__(self):
        from pathlib import Path

        self.binary = Path(self.binary)
        self.app_dir = Path(self.app_dir)
        self.config_file = Path(self.config_file)
        self.auth_dir = Path(self.auth_dir)
        self.state_dir = Path(self.state_dir)
        if not isinstance(self.repo, str) or not REPO_RE.match(self.repo):
            raise ConfigError("repository must look like owner/name: %r" % (self.repo,))
        owner, name = self.repo.split("/", 1)
        if owner in (".", "..") or name in (".", ".."):
            raise ConfigError("repository must look like owner/name: %r" % (self.repo,))
        parsed = urllib.parse.urlsplit(str(self.github_base))
        if (
            parsed.scheme != "https"
            or parsed.hostname != GITHUB_HOST
            or parsed.username
            or parsed.password
            or parsed.query
            or parsed.fragment
            or parsed.path not in ("", "/")
        ):
            raise ConfigError(
                "github_base must be https://%s with no extra path, got %r"
                % (GITHUB_HOST, self.github_base)
            )
        self.github_base = "https://%s" % GITHUB_HOST
        if not self.arch:
            self.arch = detect_arch()
        if self.arch not in ("amd64", "aarch64"):
            raise ConfigError(
                "unsupported architecture %r; expected amd64 or aarch64" % (self.arch,)
            )
        if self.http is None:
            self.http = HttpFetcher(self.http_timeout)
        if self.systemd is None:
            self.systemd = Systemd(self.unit)
        if self.sleep is None:
            self.sleep = time.sleep
        if self.probe is None:
            self.probe = probe_binary_version
        if self.health_probe is None:
            self.health_probe = make_health_probe(self.health_url, self.http_timeout)
        if self.log is None:
            self.log = Log(echo=self.echo)

    # Derived state paths ---------------------------------------------------
    @property
    def deployed_path(self):
        return self.state_dir / "deployed.json"

    @property
    def failure_path(self):
        return self.state_dir / "last_failure.json"

    @property
    def journal_path(self):
        return self.state_dir / "journal.json"

    @property
    def lock_path(self):
        return self.state_dir / "deploy.lock"

    @property
    def log_path(self):
        return self.state_dir / "deploy.log"

    @property
    def staging_dir(self):
        return self.state_dir / "staging"

    @property
    def snapshots_dir(self):
        return self.state_dir / "snapshots"

    def enable_file_logging(self):
        self.log.path = str(self.log_path)


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------


class _HttpsOnlyRedirectHandler(urllib.request.HTTPRedirectHandler):
    """Refuse redirects that downgrade to plain HTTP or have no host."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        parsed = urllib.parse.urlsplit(newurl)
        if parsed.scheme != "https" or not parsed.hostname:
            raise FetchError("refusing redirect to non-HTTPS URL %s" % newurl)
        return super().redirect_request(req, fp, code, msg, headers, newurl)


class HttpFetcher:
    def __init__(self, timeout=20.0, user_agent=USER_AGENT):
        self.timeout = float(timeout)
        self.user_agent = user_agent
        self._opener = urllib.request.build_opener(_HttpsOnlyRedirectHandler())

    @staticmethod
    def _validate_url(url):
        parsed = urllib.parse.urlsplit(str(url))
        if parsed.scheme != "https" or not parsed.hostname:
            raise FetchError("refusing non-HTTPS URL %s" % url)

    def _open(self, url, headers=None):
        self._validate_url(url)
        request_headers = {"User-Agent": self.user_agent, "Accept": "*/*"}
        if headers:
            request_headers.update(headers)
        request = urllib.request.Request(url, headers=request_headers)
        return self._opener.open(request, timeout=self.timeout)

    def get_text(self, url, max_bytes):
        try:
            with self._open(url) as response:
                data = response.read(max_bytes + 1)
        except urllib.error.HTTPError as exc:
            if exc.code == 404:
                raise NotFound("404 for %s" % url, status=404)
            raise FetchError("HTTP %s for %s" % (exc.code, url), status=exc.code)
        except Exception as exc:  # network, TLS, timeout
            raise FetchError("request failed for %s: %s" % (url, exc))
        if len(data) > max_bytes:
            raise FetchError("response from %s exceeds %d bytes" % (url, max_bytes))
        return data.decode("utf-8", "replace")

    def download(self, url, destination, max_bytes):
        """Download ``url`` to ``destination``; return (sha256, size)."""
        destination = str(destination)
        part = destination + ".part"
        digest = hashlib.sha256()
        size = 0
        try:
            with self._open(url) as response, open(part, "wb") as handle:
                while True:
                    chunk = response.read(256 * 1024)
                    if not chunk:
                        break
                    size += len(chunk)
                    if size > max_bytes:
                        raise FetchError("asset at %s exceeds %d bytes" % (url, max_bytes))
                    digest.update(chunk)
                    handle.write(chunk)
                handle.flush()
                os.fsync(handle.fileno())
        except urllib.error.HTTPError as exc:
            remove_file(part)
            if exc.code == 404:
                raise NotFound("404 for %s" % url, status=404)
            raise FetchError("HTTP %s for %s" % (exc.code, url), status=exc.code)
        except FetchError:
            remove_file(part)
            raise
        except Exception as exc:
            remove_file(part)
            raise FetchError("download failed for %s: %s" % (url, exc))
        if size == 0:
            remove_file(part)
            raise FetchError("empty asset at %s" % url)
        os.replace(part, destination)
        fsync_dir(os.path.dirname(destination) or ".")
        return digest.hexdigest(), size


# ---------------------------------------------------------------------------
# systemd, binary probing, health
# ---------------------------------------------------------------------------


class Systemd:
    def __init__(self, unit, timeout=120):
        self.unit = unit
        self.timeout = timeout

    def _run(self, arguments, timeout=None):
        try:
            return subprocess.run(
                ["systemctl"] + list(arguments),
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                timeout=timeout or self.timeout,
            )
        except FileNotFoundError:
            raise ConfigError("systemctl not found; this host does not use systemd")
        except subprocess.TimeoutExpired:
            raise DeployError("systemctl %s timed out" % " ".join(arguments))

    def is_active(self):
        return self._run(["is-active", "--quiet", self.unit], timeout=30).returncode == 0

    def is_enabled(self):
        return self._run(["is-enabled", "--quiet", self.unit], timeout=30).returncode == 0

    def restart(self):
        proc = self._run(["restart", self.unit], timeout=max(self.timeout, 180))
        if proc.returncode != 0:
            detail = (proc.stderr or proc.stdout or "").strip()
            raise DeployError(
                "systemctl restart %s failed (exit %d)%s"
                % (self.unit, proc.returncode, ": " + detail if detail else "")
            )

    def exec_start(self):
        proc = self._run(["cat", self.unit], timeout=30)
        if proc.returncode != 0:
            raise ConfigError("unit %s not found (systemctl cat failed)" % self.unit)
        for line in proc.stdout.splitlines():
            line = line.strip()
            if line.startswith("ExecStart="):
                value = line[len("ExecStart="):]
                if value.startswith("-"):
                    value = value[1:]
                try:
                    parts = shlex.split(value)
                except ValueError:
                    parts = value.split()
                return parts[0] if parts else ""
        raise ConfigError("unit %s has no ExecStart" % self.unit)

    def working_directory(self):
        proc = self._run(["show", "-p", "WorkingDirectory", "--value", self.unit], timeout=30)
        if proc.returncode != 0:
            return ""
        return proc.stdout.strip()

    def main_pid(self):
        proc = self._run(["show", "-p", "MainPID", "--value", self.unit], timeout=30)
        if proc.returncode != 0:
            return 0
        try:
            return int((proc.stdout or "").strip() or "0")
        except ValueError:
            return 0


def probe_binary_version(path):
    """Run ``<binary> -h`` in a scratch cwd and parse the version line.

    The server prints its build metadata before parsing flags, so this neither
    loads the configuration nor binds the listening port. Go's flag package
    exits non-zero for ``-h``, which is expected. The scratch working directory
    guarantees that even a future startup side effect cannot read or migrate the
    real ``config.yaml`` in the application directory.
    """
    path = str(path)
    scratch = tempfile.mkdtemp(prefix="cpap-probe-")
    try:
        proc = subprocess.run(
            [path, "-h"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=20,
            cwd=scratch,
            env=dict(os.environ, LC_ALL="C", LANG="C"),
        )
    except FileNotFoundError:
        raise ConfigError("binary not found: %s" % path)
    except PermissionError:
        raise ConfigError("binary is not executable: %s" % path)
    except subprocess.TimeoutExpired:
        raise DeployError("timed out probing version of %s" % path)
    finally:
        shutil.rmtree(scratch, ignore_errors=True)
    match = VERSION_LINE_RE.search((proc.stdout or "") + "\n" + (proc.stderr or ""))
    if not match:
        raise DeployError("could not read version from %s" % path)
    return match.group(1)


def default_process_probe(pid):
    """Return (executable realpath, sha256) for ``pid`` via /proc."""
    exe = os.readlink("/proc/%d/exe" % int(pid))
    return exe, sha256_file(exe)


def wait_for_new_process(cfg, expected_sha):
    """Wait until systemd's MainPID is the new binary and has settled.

    ``systemctl restart`` can return while the previous process is still
    finishing, so the health endpoint alone does not prove that the new
    executable is serving. Require two consecutive samples of the same main
    PID whose /proc/<pid>/exe digest equals the binary we just installed.
    """
    if not expected_sha:
        return None
    use_default = cfg.process_probe is None
    if use_default and not (os.name == "posix" and os.path.isdir("/proc")):
        cfg.log.warn("process identity check skipped: /proc is unavailable on this host")
        return None
    reader = cfg.process_probe or default_process_probe
    expected_binary = os.path.realpath(str(cfg.binary))
    stable_pid = None
    for _ in range(max(1, int(cfg.process_attempts))):
        pid = 0
        try:
            pid = int(cfg.systemd.main_pid())
        except (ConfigError, DeployError) as exc:
            cfg.log.warn("cannot read MainPID yet: %s", exc)
        if pid > 0:
            try:
                real, digest = reader(pid)
            except (OSError, DeployError) as exc:
                cfg.log.warn("cannot inspect pid %d yet: %s", pid, exc)
                real, digest = "", ""
            if (
                real
                and digest
                and os.path.realpath(real) == expected_binary
                and hmac.compare_digest(digest, expected_sha)
            ):
                if stable_pid == pid:
                    cfg.log.info("verified pid %d is running the expected binary", pid)
                    return pid
                stable_pid = pid
            else:
                stable_pid = None
        cfg.sleep(max(0.05, float(cfg.process_interval)))
    raise DeployError(
        "the restarted service did not settle on the expected binary within the process check window",
        mark_failed=True,
    )


def make_health_probe(url, timeout=5.0):
    def probe():
        request = urllib.request.Request(
            url,
            headers={"User-Agent": USER_AGENT, "Cache-Control": "no-store"},
        )
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                if getattr(response, "status", 0) != 200:
                    return False
                body = response.read(65536)
        except Exception:
            return False
        try:
            payload = json.loads(body.decode("utf-8"))
        except (ValueError, UnicodeDecodeError):
            return False
        return isinstance(payload, dict) and payload.get("status") == "ok"

    return probe


def wait_for_healthy(cfg, attempts=None, interval=None):
    attempts = attempts if attempts is not None else cfg.health_attempts
    interval = interval if interval is not None else cfg.health_interval
    active = False
    healthy = False
    for attempt in range(1, max(1, attempts) + 1):
        active = cfg.systemd.is_active()
        healthy = cfg.health_probe()
        if active and healthy:
            return True
        if attempt < attempts:
            cfg.sleep(interval)
    cfg.log.error(
        "health check failed after %d attempts (service active=%s, health endpoint ok=%s)",
        max(1, attempts),
        active,
        healthy,
    )
    return False


# ---------------------------------------------------------------------------
# Lock
# ---------------------------------------------------------------------------


class Lock:
    def __init__(self, path):
        self.path = str(path)
        self._handle = None
        self._fallback = False

    def acquire(self):
        ensure_dir(os.path.dirname(self.path) or ".")
        if fcntl is not None:
            handle = open(self.path, "a+")
            try:
                fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except OSError:
                handle.close()
                return False
            handle.seek(0)
            handle.truncate()
            handle.write(str(os.getpid()))
            handle.flush()
            self._handle = handle
            return True
        # Best-effort fallback for platforms without flock (local test runs).
        # Linux always takes the fcntl path above.
        try:
            descriptor = os.open(self.path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            return False
        os.write(descriptor, str(os.getpid()).encode("ascii"))
        os.close(descriptor)
        self._fallback = True
        return True

    def release(self):
        if self._handle is not None:
            try:
                fcntl.flock(self._handle.fileno(), fcntl.LOCK_UN)
            finally:
                self._handle.close()
                self._handle = None
        elif self._fallback:
            remove_file(self.path)
            self._fallback = False

    def is_held(self):
        """Read-only lock probe used by the inspect/status command."""
        if fcntl is None or not os.path.exists(self.path):
            return False
        try:
            handle = open(self.path, "r")
        except OSError:
            return False
        try:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            return True
        else:
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
            return False
        finally:
            handle.close()


# ---------------------------------------------------------------------------
# Release discovery
# ---------------------------------------------------------------------------


def parse_checksums(text):
    """Parse ``sha256sum`` output into {filename: digest}.

    Strict by design: malformed lines, duplicate names, or duplicate digests
    fail closed instead of silently ignoring the damage.
    """
    entries = {}
    seen_digests = {}
    for number, line in enumerate(text.splitlines(), start=1):
        stripped = line.strip()
        if not stripped:
            continue
        parts = stripped.split(None, 1)
        if len(parts) != 2:
            raise ChecksumError("malformed checksums.txt line %d" % number)
        digest, name = parts[0], parts[1].strip()
        if name.startswith("*"):
            name = name[1:]
        if not CHECKSUM_RE.match(digest):
            raise ChecksumError("invalid sha256 digest on checksums.txt line %d" % number)
        if not name or "/" in name or "\\" in name:
            raise ChecksumError("invalid asset name on checksums.txt line %d" % number)
        digest = digest.lower()
        if name in entries:
            raise ChecksumError("duplicate entry for %s in checksums.txt" % name)
        if digest in seen_digests:
            raise ChecksumError(
                "assets %s and %s share a digest in checksums.txt" % (seen_digests[digest], name)
            )
        entries[name] = digest
        seen_digests[digest] = name
    if not entries:
        raise ChecksumError("checksums.txt contains no entries")
    return entries


def api_releases_url(cfg):
    return "%s/repos/%s/releases?per_page=20" % (GITHUB_API_BASE, cfg.repo)


def api_release_url(cfg, tag):
    return "%s/repos/%s/releases/tags/%s" % (
        GITHUB_API_BASE,
        cfg.repo,
        urllib.parse.quote(tag, safe=""),
    )


def _is_published_release(payload):
    """GitHub marks a release as a formal Release via draft/prerelease=false."""
    return (
        isinstance(payload, dict)
        and payload.get("draft") is False
        and payload.get("prerelease") is False
        and bool(payload.get("published_at"))
        and isinstance(payload.get("tag_name"), str)
    )


def parse_release_list(text):
    try:
        payload = json.loads(text)
    except ValueError as exc:
        raise FetchError("invalid JSON from the GitHub releases API: %s" % exc)
    if not isinstance(payload, list):
        raise FetchError("unexpected GitHub releases API response: expected a list")
    versions = {}
    for item in payload:
        if not _is_published_release(item):
            continue
        version = parse_stable_tag(item["tag_name"])
        if version is not None:
            versions[version.tag] = version
    return sorted(versions.values(), reverse=True)


def _rate_limit_error(exc, action):
    return FetchError(
        "GitHub API returned HTTP %s while %s; this is usually a rate limit, "
        "no local changes were made - retry after the limit resets"
        % (exc.status, action),
        status=exc.status,
    )


def fetch_release_list(cfg):
    try:
        text = cfg.http.get_text(api_releases_url(cfg), MAX_API_BYTES)
    except FetchError as exc:
        if exc.status in (403, 429):
            raise _rate_limit_error(exc, "listing releases")
        raise
    return parse_release_list(text)


def fetch_release_metadata(cfg, version):
    """Prove that ``tag`` is a published, non-prerelease GitHub Release."""
    try:
        text = cfg.http.get_text(api_release_url(cfg, version.tag), MAX_API_BYTES)
    except NotFound:
        raise ConfigError(
            "release %s is not published (404 from the GitHub releases API)"
            % version.tag
        )
    except FetchError as exc:
        if exc.status in (403, 429):
            raise _rate_limit_error(exc, "reading release %s" % version.tag)
        raise
    try:
        payload = json.loads(text)
    except ValueError as exc:
        raise FetchError("invalid JSON for release %s: %s" % (version.tag, exc))
    tag_name = payload.get("tag_name") if isinstance(payload, dict) else None
    if not _is_published_release(payload) or normalize_version_text(tag_name) != version.tag:
        raise PolicyError(
            "release %s is not a published, non-prerelease stable Release" % version.tag
        )
    return payload


def fetch_checksums(cfg, tag):
    """Return a parsed checksums map, or None when checksums.txt is absent."""
    try:
        text = cfg.http.get_text(checksums_url(cfg, tag), MAX_CHECKSUM_BYTES)
    except NotFound:
        return None
    return parse_checksums(text)


def release_readiness(cfg, version, asset):
    """Return the expected archive digest when the release is deployable."""
    try:
        entries = fetch_checksums(cfg, version.tag)
    except NotFound:
        cfg.log.info("release %s: checksums.txt not published yet", version.tag)
        return None
    except ChecksumError as exc:
        cfg.log.warn("release %s: ignoring malformed checksums.txt (%s)", version.tag, exc)
        return None
    if entries is None:
        cfg.log.info("release %s: checksums.txt not published yet", version.tag)
        return None
    digest = entries.get(asset)
    if digest is None:
        cfg.log.info("release %s: checksums.txt does not list %s yet", version.tag, asset)
        return None
    return digest


@dataclasses.dataclass
class Resolution:
    version: Version
    asset: str
    expected_sha256: str


def choose_resolution(cfg, current, explicit_tag, retry, allow_major=False):
    """Pick the release to deploy, or None when the server is up to date."""
    failed_tags = read_failure_map(cfg)

    if explicit_tag:
        canonical = normalize_version_text(explicit_tag)
        version = parse_stable_tag(canonical) if canonical else None
        if version is None:
            raise ConfigError(
                "not a stable release tag: %r (expected vX.Y.Z)" % (explicit_tag,)
            )
        if current is not None and version < current:
            raise PolicyError(
                "refusing downgrade from %s to %s" % (current.tag, version.tag)
            )
        if (
            current is not None
            and version.major != current.major
            and not allow_major
        ):
            raise PolicyError(
                "refusing major-version upgrade from %s to %s; pass --allow-major "
                "to opt in on purpose" % (current.tag, version.tag)
            )
        fetch_release_metadata(cfg, version)
        if version == current:
            return None
        name = asset_name(version.tag, cfg.arch)
        digest = release_readiness(cfg, version, name)
        if digest is None:
            raise ConfigError(
                "release %s is not deployable yet: %s or its checksums.txt entry is missing"
                % (version.tag, name)
            )
        if version.tag in failed_tags and not retry:
            raise PolicyError(
                "release %s previously failed; pass --retry to attempt it again" % version.tag
            )
        return Resolution(version, name, digest)

    versions = fetch_release_list(cfg)
    if not versions:
        cfg.log.info("no published stable releases were returned by the GitHub API")
        return None
    probed = 0
    for version in versions:
        if current is not None and version < current:
            continue
        if version == current:
            break
        if current is not None and version.major != current.major and not allow_major:
            cfg.log.warn(
                "skipping %s: major upgrades require an explicit --allow-major run",
                version.tag,
            )
            continue
        if version.tag in failed_tags and not retry:
            cfg.log.warn(
                "release %s previously failed; skipping (use --retry to attempt it again)",
                version.tag,
            )
            continue
        if probed >= MAX_PROBED_RELEASES:
            cfg.log.warn("checked %d releases without finding a deployable one", probed)
            break
        probed += 1
        name = asset_name(version.tag, cfg.arch)
        digest = release_readiness(cfg, version, name)
        if digest is None:
            continue
        return Resolution(version, name, digest)
    return None


def verify_live_binary_unchanged(cfg, state):
    """Fail closed when the live binary no longer matches recorded state.

    A different digest or version means the binary was replaced or modified
    outside this tool (manual copy, another updater, partial restore). Deploying
    on top of unknown state is unsafe, so the run stops until an operator
    reconciles it (for example by re-running --baseline after inspection).
    """
    if not state:
        return 0
    expected_sha = state.get("binary_sha256")
    if not cfg.binary.is_file():
        cfg.log.error("FAIL CLOSED: recorded binary %s is missing", cfg.binary)
        return 5
    if expected_sha:
        actual = sha256_file(cfg.binary)
        if not hmac.compare_digest(actual, expected_sha):
            cfg.log.error(
                "FAIL CLOSED: live binary changed: state recorded %s but %s hashes to %s",
                expected_sha,
                cfg.binary,
                actual,
            )
            return 5
    else:
        cfg.log.warn("state has no recorded live binary digest; digest drift check skipped")
    expected_version = state.get("binary_version") or state.get("tag")
    if expected_version:
        try:
            probed = normalize_version_text(cfg.probe(cfg.binary))
        except (ConfigError, DeployError) as exc:
            cfg.log.error("FAIL CLOSED: cannot probe the live binary: %s", exc)
            return 5
        if probed != expected_version:
            cfg.log.error(
                "FAIL CLOSED: live binary reports %r but state recorded %s",
                probed,
                expected_version,
            )
            return 5
    return 0


def assert_unit_points_to_binary(cfg):
    """Refuse to deploy when the unit no longer starts the reviewed path.

    A unit edited after installation can change both ExecStart and
    WorkingDirectory. The latter matters even when the binary path is
    unchanged: a relative --config or auth path is resolved against the
    service working directory, so deploying with a drifted directory could
    make the new binary read a different configuration. Missing is rejected
    too; the installer requires an explicit WorkingDirectory.
    """
    try:
        exec_start = cfg.systemd.exec_start()
    except (ConfigError, DeployError) as exc:
        raise ConfigError("cannot read the unit ExecStart: %s" % exc)
    if not exec_start:
        raise ConfigError("unit %s has no ExecStart" % cfg.unit)
    if os.path.realpath(exec_start) != os.path.realpath(str(cfg.binary)):
        raise ConfigError(
            "unit %s ExecStart is %s, not %s; refusing to replace a binary the "
            "service does not start" % (cfg.unit, exec_start, cfg.binary)
        )
    try:
        working_dir = cfg.systemd.working_directory()
    except (ConfigError, DeployError) as exc:
        raise ConfigError("cannot read the unit WorkingDirectory: %s" % exc)
    if not working_dir:
        raise ConfigError(
            "unit %s has no WorkingDirectory; expected %s; refusing to deploy "
            "because relative config paths could resolve elsewhere"
            % (cfg.unit, cfg.app_dir)
        )
    if os.path.realpath(working_dir) != os.path.realpath(str(cfg.app_dir)):
        raise ConfigError(
            "unit %s WorkingDirectory is %s, not %s; refusing to deploy because "
            "relative config paths could resolve elsewhere"
            % (cfg.unit, working_dir, cfg.app_dir)
        )


def verify_deployed_release_digest(cfg, current, state):
    """Fail closed when an already-deployed tag's asset digest changed.

    GitHub releases are treated as immutable. If the published checksum for
    the asset we installed no longer matches the value recorded at deploy
    time, refuse to continue: a release was republished under the same tag.
    """
    if current is None:
        return 0
    if not state:
        return 0
    recorded = state.get("sha256")
    if not recorded:
        cfg.log.info("no recorded archive digest for %s; integrity check skipped", current.tag)
        return 0
    name = state.get("asset") or asset_name(current.tag, cfg.arch)
    try:
        entries = fetch_checksums(cfg, current.tag)
    except NotFound:
        cfg.log.error(
            "FAIL CLOSED: checksums.txt for deployed tag %s is no longer available", current.tag
        )
        return 5
    except FetchError as exc:
        cfg.log.warn("cannot verify %s against GitHub right now: %s", current.tag, exc)
        return 0
    except ChecksumError as exc:
        cfg.log.error("FAIL CLOSED: checksums.txt for %s is malformed: %s", current.tag, exc)
        return 5
    if entries is None:
        cfg.log.error(
            "FAIL CLOSED: checksums.txt for deployed tag %s is no longer available", current.tag
        )
        return 5
    published = entries.get(name)
    if published is None:
        cfg.log.error("FAIL CLOSED: deployed asset %s is no longer listed for %s", name, current.tag)
        return 5
    if not hmac.compare_digest(published, recorded):
        cfg.log.error(
            "FAIL CLOSED: release %s changed: recorded %s but published %s for %s",
            current.tag,
            recorded,
            published,
            name,
        )
        return 5
    cfg.log.info("release %s integrity check passed", current.tag)
    return 0


# ---------------------------------------------------------------------------
# Archive handling
# ---------------------------------------------------------------------------


def _validate_members(members):
    binary = None
    for member in members:
        name = member.name
        if not name or "\x00" in name or "\\" in name:
            raise DeployError("archive contains an invalid member name", mark_failed=True)
        if name.startswith("/") or name.startswith("../") or "/../" in "/" + name:
            raise DeployError(
                "archive member %r uses an unsafe path" % name, mark_failed=True
            )
        parts = name.split("/")
        if any(part in ("", ".", "..") for part in parts):
            raise DeployError(
                "archive member %r uses an unsafe path" % name, mark_failed=True
            )
        if member.issym() or member.islnk():
            raise DeployError(
                "archive member %r is a link; refusing to extract" % name, mark_failed=True
            )
        if member.ischr() or member.isblk() or member.isfifo() or member.isdev():
            raise DeployError(
                "archive member %r has an unsafe type" % name, mark_failed=True
            )
        if not (member.isreg() or member.isdir()):
            raise DeployError(
                "archive member %r has an unexpected type" % name, mark_failed=True
            )
        if name == BINARY_NAME:
            if binary is not None:
                raise DeployError("archive contains duplicate %s members" % BINARY_NAME, mark_failed=True)
            if not member.isreg():
                raise DeployError("%s is not a regular file in the archive" % BINARY_NAME, mark_failed=True)
            if member.size <= 0 or member.size > MAX_BINARY_BYTES:
                raise DeployError(
                    "archive binary size %d is outside the allowed range" % member.size,
                    mark_failed=True,
                )
            binary = member
    if binary is None:
        raise DeployError("archive does not contain %s" % BINARY_NAME, mark_failed=True)
    return binary


def extract_binary(archive_path, destination, max_bytes=MAX_BINARY_BYTES):
    """Extract only the regular ``cli-proxy-api`` member to ``destination``.

    All members are validated first (no links, no absolute paths, no ``..``).
    Extraction is written by this function, never by ``tarfile.extract*``.
    """
    destination = str(destination)
    part = destination + ".part"
    try:
        with tarfile.open(str(archive_path), mode="r:gz") as archive:
            binary = _validate_members(archive.getmembers())
            if binary.size > max_bytes:
                raise DeployError("archive binary is too large", mark_failed=True)
            source = archive.extractfile(binary)
            if source is None:
                raise DeployError("cannot read %s from archive" % BINARY_NAME, mark_failed=True)
            descriptor = os.open(part, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o755)
            try:
                remaining = binary.size
                while remaining > 0:
                    chunk = source.read(min(1024 * 1024, remaining))
                    if not chunk:
                        break
                    os.write(descriptor, chunk)
                    remaining -= len(chunk)
                if remaining != 0:
                    raise DeployError("archive is truncated", mark_failed=True)
                os.fsync(descriptor)
            finally:
                os.close(descriptor)
        os.replace(part, destination)
        fsync_dir(os.path.dirname(destination) or ".")
    except DeployError:
        remove_file(part)
        raise
    except (tarfile.TarError, EOFError, OSError) as exc:
        remove_file(part)
        raise DeployError("cannot extract release archive: %s" % exc, mark_failed=True)
    return destination


# ---------------------------------------------------------------------------
# Journal and state helpers
# ---------------------------------------------------------------------------


def read_journal(cfg):
    """Read the transaction journal, mapping decode damage to RecoveryError."""
    try:
        journal = read_json(cfg.journal_path)
    except (ValueError, OSError) as exc:
        raise RecoveryError(
            "journal %s is unreadable or is not valid JSON: %s" % (cfg.journal_path, exc)
        )
    if journal is None:
        return None
    if not isinstance(journal, dict):
        raise RecoveryError("journal %s is malformed; inspect it manually" % cfg.journal_path)
    return journal


def read_deployed_state(cfg):
    try:
        state = read_json(cfg.deployed_path)
    except (ValueError, OSError) as exc:
        raise RecoveryError(
            "deployed state %s is unreadable or is not valid JSON: %s"
            % (cfg.deployed_path, exc)
        )
    if state is None:
        return None
    if not isinstance(state, dict) or not state.get("tag"):
        raise RecoveryError("malformed %s; remove it to re-baseline" % cfg.deployed_path)
    return state


def read_failure_map(cfg):
    """Return {tag: failure record}, accepting the legacy single-record format.

    A corrupt failure file is ignored (with a warning) rather than blocking
    recovery: failing to read bookkeeping must not prevent a safe rollback.
    """
    try:
        payload = read_json(cfg.failure_path)
    except (ValueError, OSError) as exc:
        cfg.log.warn("ignoring unreadable failure record %s: %s", cfg.failure_path, exc)
        return {}
    if not isinstance(payload, dict):
        return {}
    failures = payload.get("failures")
    if isinstance(failures, dict):
        return {
            tag: record
            for tag, record in failures.items()
            if isinstance(tag, str) and isinstance(record, dict)
        }
    tag = payload.get("tag")
    if isinstance(tag, str) and tag:
        return {tag: payload}
    return {}


def mark_failure(cfg, tag, reason, rolled_back):
    failures = read_failure_map(cfg)
    failures[tag] = {
        "tag": tag,
        "reason": reason,
        "rolled_back": bool(rolled_back),
        "at": utcnow(),
    }
    ordered = sorted(
        failures.values(), key=lambda item: item.get("at") or "", reverse=True
    )[:MAX_FAILED_TAGS]
    payload = {
        "schema": STATE_SCHEMA,
        "failures": {item["tag"]: item for item in ordered},
    }
    atomic_write_json(cfg.failure_path, payload)
    cfg.log.error("recorded failed release %s (rolled_back=%s): %s", tag, rolled_back, reason)


def clear_failure(cfg, tag):
    failures = read_failure_map(cfg)
    if tag not in failures:
        return
    failures.pop(tag, None)
    if not failures:
        remove_file(cfg.failure_path)
        cfg.log.info("cleared previous failure for %s", tag)
        return
    payload = {"schema": STATE_SCHEMA, "failures": failures}
    atomic_write_json(cfg.failure_path, payload)
    cfg.log.info("cleared previous failure for %s", tag)


def restore_deployed_state(cfg, previous_state):
    """Put the pre-transaction state back as part of a rollback."""
    if previous_state is None:
        remove_file(cfg.deployed_path)
        try:
            fsync_dir(str(cfg.state_dir))
        except OSError:
            pass
        cfg.log.warn("rollback removed the deployment state (none existed before this run)")
        return
    if not isinstance(previous_state, dict) or not previous_state.get("tag"):
        raise RecoveryError("journal contains a malformed previous deployment state")
    atomic_write_json(cfg.deployed_path, previous_state)
    cfg.log.warn("rollback restored deployment state to %s", previous_state.get("tag"))


def write_deployed_state(
    cfg,
    tag,
    asset,
    archive_sha256,
    previous_tag,
    binary_sha256,
    config_snapshot=None,
    binary_version=None,
):
    payload = {
        "schema": STATE_SCHEMA,
        "tag": tag,
        "asset": asset,
        "sha256": archive_sha256,
        "binary_sha256": binary_sha256,
        "binary_version": binary_version or tag,
        "previous_tag": previous_tag,
        "config_snapshot": config_snapshot,
        "deployed_at": utcnow(),
        "source": "release",
    }
    atomic_write_json(cfg.deployed_path, payload)
    cfg.log.info("state: deployed %s (previous %s)", tag, previous_tag or "none")


def prune_backups(cfg, protect=None):
    keep = max(1, int(cfg.keep_backups))
    protected = os.path.realpath(str(protect)) if protect else None
    candidates = []
    for name in os.listdir(str(cfg.app_dir)):
        if not name.startswith(BACKUP_PREFIX):
            continue
        path = cfg.app_dir / name
        if not path.is_file() or path.is_symlink():
            continue
        if protected and os.path.realpath(str(path)) == protected:
            continue
        candidates.append(path)
    candidates.sort(key=lambda item: os.stat(str(item)).st_mtime, reverse=True)
    other_limit = max(0, keep - 1) if protected else keep
    kept = len(candidates) + (1 if protected else 0)
    removed = 0
    for path in candidates[other_limit:]:
        remove_file(path)
        removed += 1
    if removed:
        cfg.log.info("pruned %d old binary backups (kept %d)", removed, min(kept, keep))
    return removed


def snapshot_config(cfg, label):
    """Copy the live config.yaml to the state dir for manual comparison.

    This is a read-only copy: the updater never writes, normalizes, or migrates
    the application configuration. The snapshot lets an operator compare or
    manually restore the pre-deployment file if the new binary migrates the
    configuration at startup. Snapshots contain the same secrets as config.yaml
    and are therefore created mode 0600 inside the root-only state directory.
    """
    source = cfg.config_file
    if not source.is_file():
        cfg.log.info("no config file at %s; skipping configuration snapshot", source)
        return None
    ensure_dir(str(cfg.snapshots_dir), 0o700)
    stamp = utcnow().replace("-", "").replace(":", "")
    safe_label = re.sub(r"[^A-Za-z0-9._-]+", "_", label or "unknown")
    destination = cfg.snapshots_dir / ("config.%s.%s.yaml" % (safe_label, stamp))
    shutil.copy2(str(source), str(destination))
    with open(str(destination), "r+b") as handle:
        os.fsync(handle.fileno())
    if os.name == "posix":
        os.chmod(str(destination), 0o600)
    cfg.log.info("saved read-only config snapshot to %s", destination)
    return str(destination)


def cleanup_staged(cfg):
    """Remove orphaned staging files left behind by a crashed run."""
    try:
        names = os.listdir(str(cfg.app_dir))
    except FileNotFoundError:
        return
    for name in names:
        if name.startswith(STAGED_PREFIX) or name.startswith(ROLLBACK_PREFIX):
            path = cfg.app_dir / name
            if path.is_file():
                remove_file(path)
                cfg.log.warn("removed orphaned temporary file %s", path)


# ---------------------------------------------------------------------------
# Transaction: commit, rollback, recovery
# ---------------------------------------------------------------------------


def commit_transaction(cfg, journal):
    """Make the new deployment authoritative, then do only non-critical cleanup.

    Once ``deployed.json`` has been written, a cleanup failure (failure-record
    update, journal removal, backup pruning, or logging) must never trigger a
    rollback of the committed state. The journal is left in place for the next
    run to finish idempotently instead.
    """
    write_deployed_state(
        cfg,
        journal["tag"],
        journal.get("asset"),
        journal.get("archive_sha256"),
        journal.get("previous_tag"),
        journal.get("new_binary_sha256"),
        journal.get("config_snapshot"),
        journal.get("binary_version") or journal["tag"],
    )
    try:
        clear_failure(cfg, journal["tag"])
    except Exception as exc:  # noqa: BLE001 - post-commit cleanup is best effort
        try:
            cfg.log.warn("state committed but clearing the failure record failed: %s", exc)
        except Exception:
            pass
    try:
        remove_file(cfg.journal_path)
    except Exception as exc:  # noqa: BLE001
        try:
            cfg.log.warn("state committed but removing the journal failed: %s", exc)
        except Exception:
            pass
    try:
        prune_backups(cfg, protect=journal.get("backup_path"))
    except Exception as exc:  # noqa: BLE001
        try:
            cfg.log.warn("state committed but backup pruning failed: %s", exc)
        except Exception:
            pass


def _record_rollback_intent(cfg, journal, reason):
    """Persist the rollback phase and failure BEFORE touching the live binary."""
    journal["phase"] = "rolling_back"
    journal["reason"] = reason
    try:
        atomic_write_json(cfg.journal_path, journal)
    except OSError as exc:
        cfg.log.error("cannot persist rollback intent; refusing to touch the binary: %s", exc)
        return False
    try:
        mark_failure(cfg, journal.get("tag"), reason, rolled_back=False)
    except OSError as exc:
        cfg.log.warn("cannot update the failure record before rollback: %s", exc)
    return True


def _record_rollback_failure(cfg, journal, reason, binary_restored):
    """Retain an unverified rollback journal so the next run stays fail-closed."""
    journal["phase"] = "rollback_failed"
    journal["reason"] = reason
    try:
        atomic_write_json(cfg.journal_path, journal)
    except OSError as exc:
        cfg.log.error("cannot persist failed rollback state: %s", exc)
    try:
        mark_failure(
            cfg,
            journal.get("tag"),
            reason,
            rolled_back=binary_restored,
        )
    except OSError as exc:
        cfg.log.error("cannot record the failed rollback: %s", exc)


def _restart_and_verify_rollback(cfg, journal):
    try:
        cfg.systemd.restart()
        if not wait_for_healthy(cfg):
            return False
        wait_for_new_process(cfg, journal.get("backup_sha256"))
        return True
    except DeployError as exc:
        cfg.log.error("rollback restart or health verification failed: %s", exc)
        return False


def _finish_successful_rollback(cfg, journal, reason):
    try:
        restore_deployed_state(cfg, journal.get("previous_state"))
    except (OSError, RecoveryError) as exc:
        _record_rollback_failure(
            cfg,
            journal,
            "%s; the previous binary is restored but restoring deployed.json failed: %s"
            % (reason, exc),
            binary_restored=True,
        )
        return False
    try:
        mark_failure(cfg, journal.get("tag"), reason, rolled_back=True)
    except OSError as exc:
        cfg.log.warn("rollback succeeded but recording the failure failed: %s", exc)
    try:
        remove_file(cfg.journal_path)
    except OSError as exc:
        cfg.log.warn("rollback succeeded but removing the journal failed: %s", exc)
    cfg.log.warn("rolled back %s; previous binary is healthy again", journal.get("tag"))
    return True


def rollback_transaction(cfg, journal, reason):
    tag = journal.get("tag")
    if not tag or not journal.get("backup_sha256"):
        raise RecoveryError("cannot roll back a journal without a tag and backup digest")
    cfg.log.error("deployment of %s failed: %s", tag, reason)
    if not _record_rollback_intent(cfg, journal, reason):
        _record_rollback_failure(
            cfg, journal, "could not persist rollback intent: %s" % reason, False
        )
        return False

    backup = journal.get("backup_path")
    try:
        if not backup or not os.path.isfile(str(backup)):
            raise OSError("backup file %r is missing" % backup)
        actual = sha256_file(backup)
        if actual != journal["backup_sha256"]:
            raise OSError("backup digest changed while deploying")
        tmp = str(cfg.app_dir / (ROLLBACK_PREFIX + str(os.getpid())))
        shutil.copy2(backup, tmp)
        with open(tmp, "r+b") as handle:
            os.fsync(handle.fileno())
        os.replace(tmp, str(cfg.binary))
        fsync_dir(str(cfg.app_dir))
        cfg.log.step("restored previous binary from %s", backup)
    except (OSError, ValueError) as exc:
        cfg.log.error("ROLLBACK FAILED: %s", exc)
        _record_rollback_failure(
            cfg, journal, "rollback failed: %s" % exc, binary_restored=False
        )
        return False

    if not _restart_and_verify_rollback(cfg, journal):
        _record_rollback_failure(
            cfg,
            journal,
            reason + "; rollback health check failed",
            binary_restored=True,
        )
        return False
    return _finish_successful_rollback(cfg, journal, reason)


def _recovery_process_ok(cfg, journal, expected_sha):
    try:
        wait_for_new_process(cfg, expected_sha)
        return True
    except DeployError as exc:
        cfg.log.error("process identity check during recovery failed: %s", exc)
        return False


def recover_transaction(cfg):
    """Resolve a journal left by a crashed run. Returns a short outcome string."""
    journal = read_journal(cfg)
    if not journal:
        return None
    if not journal.get("tag"):
        raise RecoveryError("journal %s is malformed; inspect it manually" % cfg.journal_path)
    phase = journal.get("phase")
    if phase in ("failed", "rollback_failed"):
        raise RecoveryError(
            "previous transaction for %s failed and its rollback is unverified; "
            "inspect the server, then use --abort-journal only after resolving it"
            % journal.get("tag")
        )
    if phase not in ("prepared", "swapped", "rolling_back"):
        raise RecoveryError(
            "journal for %s has unknown phase %r; inspect it manually"
            % (journal.get("tag"), phase)
        )

    new_sha = journal.get("new_binary_sha256")
    backup_sha = journal.get("backup_sha256")
    if not new_sha or not backup_sha:
        raise RecoveryError("journal is missing binary digests; inspect it manually")
    if not cfg.binary.is_file():
        raise RecoveryError("live binary %s is missing; manual intervention required" % cfg.binary)
    live_sha = sha256_file(cfg.binary)

    # Read the live binary BEFORE requiring the backup: a prepared journal whose
    # live file still hashes to the backup is provably unswapped, so a missing
    # backup cannot matter and the journal can be discarded safely.
    if phase == "prepared" and hmac.compare_digest(live_sha, backup_sha) and not hmac.compare_digest(
        live_sha, new_sha
    ):
        cfg.log.warn(
            "recovering interrupted transaction for %s: swap never happened; discarding journal",
            journal.get("tag"),
        )
        staged = journal.get("staged_path")
        if staged and os.path.isfile(str(staged)):
            remove_file(staged)
        remove_file(cfg.journal_path)
        return "discarded"

    backup = journal.get("backup_path")
    if not backup or not os.path.isfile(str(backup)):
        raise RecoveryError(
            "journal for %s is present but its backup %r is missing; manual intervention required"
            % (journal.get("tag"), backup)
        )
    if sha256_file(backup) != backup_sha:
        raise RecoveryError(
            "backup %s does not match the digest recorded in the journal; manual intervention required"
            % backup
        )

    if hmac.compare_digest(live_sha, new_sha):
        if phase == "rolling_back":
            cfg.log.warn(
                "recovering interrupted rollback for %s: the new binary is still live; retrying rollback",
                journal.get("tag"),
            )
            if rollback_transaction(
                cfg, journal, "interrupted rollback resumed after a crash"
            ):
                return "rolled_back"
            raise RecoveryError(
                "rollback for %s is still unverified; the journal was retained" % journal.get("tag")
            )
        cfg.log.warn(
            "recovering interrupted transaction for %s: the new binary is already installed",
            journal.get("tag"),
        )
        try:
            cfg.systemd.restart()
        except DeployError as exc:
            cfg.log.error("restart during recovery failed: %s", exc)
        if wait_for_healthy(cfg) and _recovery_process_ok(cfg, journal, new_sha):
            commit_transaction(cfg, journal)
            cfg.log.info("recovered interrupted transaction: committed %s", journal.get("tag"))
            return "committed"
        if rollback_transaction(
            cfg, journal, "interrupted transaction did not pass the health check"
        ):
            return "rolled_back"
        raise RecoveryError(
            "interrupted transaction for %s could not be rolled back safely; journal retained"
            % journal.get("tag")
        )

    if hmac.compare_digest(live_sha, backup_sha):
        # The old binary is live but rollback never finished (swapped/rolling_back).
        # Verify it, restore the previous state, and never redeploy the failed tag.
        if rollback_transaction(cfg, journal, "interrupted rollback resumed after a crash"):
            return "rolled_back"
        raise RecoveryError(
            "rollback for %s is still unverified; the journal was retained" % journal.get("tag")
        )

    raise RecoveryError(
        "live binary digest %s matches neither the new binary nor the backup recorded for %s; "
        "manual intervention required" % (live_sha[:12], journal.get("tag"))
    )


# ---------------------------------------------------------------------------
# Deployment
# ---------------------------------------------------------------------------


def resolve_current_version(cfg, state):
    tag = state.get("tag") if state else None
    version = parse_stable_tag(tag) if tag else None
    if version is not None:
        return version
    if tag:
        cfg.log.warn("recorded tag %r is not a stable version; probing the live binary", tag)
    try:
        probed = normalize_version_text(cfg.probe(cfg.binary))
    except (ConfigError, DeployError) as exc:
        cfg.log.warn("cannot determine the running version: %s", exc)
        return None
    if probed is None:
        cfg.log.warn("live binary version is not a stable vX.Y.Z value")
        return None
    cfg.log.warn("no recorded deployment; using live binary version %s as the baseline", probed)
    return parse_stable_tag(probed)


def plan_text(cfg, current, resolution):
    lines = []
    lines.append(
        "plan: upgrade %s -> %s" % (current.tag if current else "unknown baseline", resolution.version.tag)
    )
    lines.append("  release:   %s/%s/releases/tag/%s" % (cfg.github_base, cfg.repo, resolution.version.tag))
    lines.append("  asset:     %s" % resolution.asset)
    lines.append("  sha256:    %s" % resolution.expected_sha256)
    lines.append("  service:   %s (%s)" % (cfg.unit, cfg.health_url))
    lines.append(
        "  steps:     download -> checksum -> safe extract -> version preflight"
        " -> config snapshot -> backup -> journal -> atomic replace"
        " -> systemctl restart -> health check"
    )
    return "\n".join(lines)


def deploy_resolution(cfg, resolution, current, dry_run):
    if dry_run:
        print(plan_text(cfg, current, resolution))
        print("dry run: no files were changed and the asset was not downloaded")
        return 0
    if not is_root() and not cfg.allow_non_root:
        raise ConfigError("deployment requires root; run via sudo or as root")
    assert_unit_points_to_binary(cfg)
    live_sha_before = sha256_file(cfg.binary)

    log = cfg.log
    cfg.enable_file_logging()
    staging = cfg.staging_dir / resolution.version.tag
    staged = cfg.app_dir / (STAGED_PREFIX + str(os.getpid()))
    phase = "preflight"

    def fail(message, mark_failed):
        raise DeployError(message, mark_failed=mark_failed)

    try:
        ensure_dir(str(staging), 0o700)
        archive = staging / resolution.asset
        log.step("downloading %s", release_url(cfg, resolution.version.tag, resolution.asset))
        try:
            digest, size = cfg.http.download(
                release_url(cfg, resolution.version.tag, resolution.asset),
                archive,
                MAX_ASSET_BYTES,
            )
        except NotFound:
            fail("asset disappeared during download; will retry on the next run", False)
        except FetchError as exc:
            fail("download failed: %s" % exc, False)
        if not hmac.compare_digest(digest, resolution.expected_sha256):
            fail(
                "sha256 mismatch for %s: downloaded %s, checksums.txt says %s"
                % (resolution.asset, digest, resolution.expected_sha256),
                True,
            )
        log.step("verified %s (%d bytes)", resolution.asset, size)

        log.step("extracting %s with strict member validation", BINARY_NAME)
        extract_binary(archive, staged)

        log.step("preflight: checking staged binary version")
        try:
            probed = cfg.probe(staged)
        except (ConfigError, DeployError) as exc:
            fail("cannot execute the staged binary: %s" % exc, True)
        if normalize_version_text(probed) != resolution.version.tag:
            fail(
                "staged binary reports version %r, expected %s"
                % (probed, resolution.version.version),
                True,
            )
        new_sha = sha256_file(staged)

        # Re-check immediately before touching the live binary: the unit or the
        # live file may have changed while the archive was downloading.
        assert_unit_points_to_binary(cfg)
        if sha256_file(cfg.binary) != live_sha_before:
            raise RecoveryError(
                "the live binary changed while the release was downloading; refusing "
                "to replace a file this run did not verify"
            )

        config_snapshot = snapshot_config(cfg, resolution.version.tag)

        live_stat = os.stat(str(cfg.binary))
        live_mode = stat.S_IMODE(live_stat.st_mode)
        os.chmod(staged, live_mode)
        try:
            os.chown(staged, live_stat.st_uid, live_stat.st_gid)
        except (AttributeError, PermissionError, OSError):
            pass

        backup = cfg.app_dir / (BACKUP_PREFIX + resolution.version.tag)
        shutil.copy2(str(cfg.binary), str(backup))
        os.utime(str(backup), None)
        with open(str(backup), "r+b") as handle:
            os.fsync(handle.fileno())
        fsync_dir(str(cfg.app_dir))
        backup_sha = sha256_file(backup)
        log.step("backed up live binary to %s", backup)
        previous_state = read_deployed_state(cfg)

        journal = {
            "schema": STATE_SCHEMA,
            "phase": "prepared",
            "tag": resolution.version.tag,
            "previous_tag": current.tag if current else None,
            "previous_state": previous_state,
            "asset": resolution.asset,
            "archive_sha256": digest,
            "new_binary_sha256": new_sha,
            "binary_version": resolution.version.tag,
            "backup_path": str(backup),
            "backup_sha256": backup_sha,
            "live_path": str(cfg.binary),
            "staged_path": str(staged),
            "config_snapshot": config_snapshot,
            "mode": live_mode,
            "started_at": utcnow(),
            "pid": os.getpid(),
        }
        atomic_write_json(cfg.journal_path, journal)
        log.step("journal written; replacing the live binary atomically")
        phase = "swap"
        os.replace(str(staged), str(cfg.binary))
        fsync_dir(str(cfg.app_dir))
        journal["phase"] = "swapped"
        atomic_write_json(cfg.journal_path, journal)

        log.step("restarting %s", cfg.unit)
        cfg.systemd.restart()
        if not wait_for_healthy(cfg):
            fail("health check failed after restart", True)
        wait_for_new_process(cfg, new_sha)

        commit_transaction(cfg, journal)
        phase = "committed"
        log.info(
            "deployed %s (%s) successfully; backup %s kept",
            resolution.version.tag,
            resolution.asset,
            backup.name,
        )
        return 0
    except (DeployError, OSError) as exc:
        log.error("%s", exc)
        mark_failed = getattr(exc, "mark_failed", phase == "swap")
        if phase == "committed":
            # The new binary and deployed.json are authoritative. Never roll
            # back because of a post-commit cleanup or logging failure.
            log.error("deployment committed; ignoring post-commit error: %s", exc)
            return 1
        if phase == "swap":
            journal = read_journal(cfg)
            if not journal:
                log.error(
                    "transaction failed but its journal is missing; refusing to guess how to recover"
                )
                return 5
            if rollback_transaction(cfg, journal, str(exc)):
                return 1
            log.error("rollback did not complete; journal retained for the next run")
            return 5
        if mark_failed:
            mark_failure(cfg, resolution.version.tag, str(exc), rolled_back=False)
        return 1
    finally:
        remove_file(staged)
        shutil.rmtree(str(staging), ignore_errors=True)


def run_deploy(cfg, explicit_tag=None, retry=False, dry_run=False, allow_major=False):
    state = read_deployed_state(cfg)
    current = resolve_current_version(cfg, state)

    if dry_run:
        journal = read_journal(cfg)
        if journal:
            print(
                "warning: an interrupted transaction is pending for %s; a real run recovers it first"
                % journal.get("tag")
            )
            if journal.get("phase") in ("failed", "rollback_failed"):
                print("error: the pending transaction needs manual intervention")
                return 5
        else:
            drift = verify_live_binary_unchanged(cfg, state)
            if drift:
                return drift
        resolution = choose_resolution(
            cfg, current, explicit_tag, retry, allow_major=allow_major
        )
        if resolution is None:
            print("no newer deployable release (current: %s)" % (current.tag if current else "unknown"))
            return verify_deployed_release_digest(cfg, current, state)
        return deploy_resolution(cfg, resolution, current, dry_run=True)

    if not is_root() and not cfg.allow_non_root:
        raise ConfigError("deployment requires root; run via sudo or as root")
    if not cfg.binary.is_file():
        raise ConfigError("live binary not found: %s" % cfg.binary)
    if not os.access(str(cfg.binary), os.X_OK):
        raise ConfigError("live binary is not executable: %s" % cfg.binary)

    ensure_dir(str(cfg.state_dir), 0o700)
    cfg.enable_file_logging()
    lock = Lock(cfg.lock_path)
    if not lock.acquire():
        cfg.log.warn("another deployment run holds %s; exiting", cfg.lock_path)
        return 4
    try:
        outcome = recover_transaction(cfg)
        if outcome == "rolled_back":
            return 1
        state = read_deployed_state(cfg)
        current = resolve_current_version(cfg, state)
        cfg.log.info(
            "current=%s latest check starting (arch=%s unit=%s)",
            current.tag if current else "unknown",
            cfg.arch,
            cfg.unit,
        )
        assert_unit_points_to_binary(cfg)
        drift = verify_live_binary_unchanged(cfg, state)
        if drift:
            return drift
        cleanup_staged(cfg)
        resolution = choose_resolution(
            cfg, current, explicit_tag, retry, allow_major=allow_major
        )
        if resolution is None:
            if explicit_tag:
                cfg.log.info("release %s is already the deployed version", explicit_tag)
            return verify_deployed_release_digest(cfg, current, state)
        return deploy_resolution(cfg, resolution, current, dry_run=False)
    finally:
        lock.release()


def abort_journal(cfg):
    if not is_root() and not cfg.allow_non_root:
        raise ConfigError("this operation requires root")
    ensure_dir(str(cfg.state_dir), 0o700)
    cfg.enable_file_logging()
    lock = Lock(cfg.lock_path)
    if not lock.acquire():
        cfg.log.warn("another deployment run holds the lock; not touching the journal")
        return 4
    try:
        journal = read_journal(cfg)
        if not journal:
            print("no pending journal")
            return 0
        cfg.log.warn(
            "discarding journal for %s at operator request; the live binary is NOT touched",
            journal.get("tag"),
        )
        remove_file(cfg.journal_path)
        return 0
    finally:
        lock.release()


def record_baseline(cfg, explicit_version):
    if not is_root() and not cfg.allow_non_root:
        raise ConfigError("recording the baseline requires root")
    if not cfg.binary.is_file():
        raise ConfigError("live binary not found: %s" % cfg.binary)
    existing = read_deployed_state(cfg)
    ensure_dir(str(cfg.state_dir), 0o700)
    cfg.enable_file_logging()
    lock = Lock(cfg.lock_path)
    if not lock.acquire():
        cfg.log.warn("another deployment run holds the lock; try again")
        return 4
    try:
        if existing:
            cfg.log.info("baseline already recorded: %s (nothing to do)", existing.get("tag"))
            return 0
        tag = None
        if explicit_version:
            tag = normalize_version_text(explicit_version)
            if tag is None:
                raise ConfigError(
                    "invalid --baseline-version %r; expected vX.Y.Z" % explicit_version
                )
        else:
            try:
                tag = normalize_version_text(cfg.probe(cfg.binary))
            except (ConfigError, DeployError) as exc:
                cfg.log.warn("cannot determine the running version: %s", exc)
                tag = None
        if tag is None:
            raise ConfigError(
                "cannot determine the running version; pass --baseline-version vX.Y.Z"
            )
        binary_sha = sha256_file(cfg.binary)
        config_snapshot = snapshot_config(cfg, tag)
        payload = {
            "schema": STATE_SCHEMA,
            "tag": tag,
            "asset": None,
            "sha256": None,
            "binary_sha256": binary_sha,
            "binary_version": tag,
            "previous_tag": None,
            "config_snapshot": config_snapshot,
            "deployed_at": utcnow(),
            "source": "baseline",
        }
        atomic_write_json(cfg.deployed_path, payload)
        cfg.log.info("recorded baseline %s (no upgrade was performed)", tag)
        return 0
    finally:
        lock.release()


# ---------------------------------------------------------------------------
# Status and environment checks
# ---------------------------------------------------------------------------


def _print_line(level, label, detail=""):
    line = "%-5s %-32s %s" % (level, label, detail)
    print(line.rstrip(), flush=True)


def inspect_status(cfg):
    print("CLIProxyAPI release-pull deploy status")
    print("  state dir:      %s" % cfg.state_dir)
    state = None
    try:
        state = read_deployed_state(cfg)
    except (ConfigError, RecoveryError) as exc:
        _print_line("FAIL", "state", str(exc))
    if state:
        print(
            "  deployed:       %s (source=%s, at=%s)"
            % (state.get("tag"), state.get("source") or "release", state.get("deployed_at"))
        )
        print("  asset:          %s" % (state.get("asset") or "-"))
        print("  archive sha256: %s" % (state.get("sha256") or "- (baseline)"))
        print("  previous:       %s" % (state.get("previous_tag") or "-"))
        print("  config snapshot:%s" % (" " + state.get("config_snapshot") if state.get("config_snapshot") else " -"))
    else:
        print("  deployed:       unknown (no state file)")
    failures = read_failure_map(cfg)
    if failures:
        for tag in sorted(failures):
            record = failures[tag]
            print(
                "  failed release: %s at %s (rolled_back=%s): %s"
                % (tag, record.get("at"), record.get("rolled_back"), record.get("reason"))
            )
    else:
        print("  failures:       none")
    try:
        journal = read_journal(cfg)
    except RecoveryError as exc:
        print("  journal:        unreadable (%s)" % exc)
        journal = None
    if journal:
        print("  journal:        phase=%s tag=%s" % (journal.get("phase"), journal.get("tag")))
    elif cfg.journal_path.exists():
        print("  journal:        unreadable")
    else:
        print("  journal:        none")

    binary = cfg.binary
    if binary.is_file():
        st = os.stat(str(binary))
        print(
            "  binary:         %s (mode=%04o, size=%d)"
            % (binary, stat.S_IMODE(st.st_mode), st.st_size)
        )
        try:
            print("  binary sha256:  %s" % sha256_file(binary))
        except OSError as exc:
            print("  binary sha256:  unreadable (%s)" % exc)
        try:
            print("  binary version: %s" % normalize_version_text(cfg.probe(binary)))
        except (ConfigError, DeployError) as exc:
            print("  binary version: unknown (%s)" % exc)
    else:
        print("  binary:         missing (%s)" % binary)

    print("  config:         %s (exists=%s)" % (cfg.config_file, cfg.config_file.exists()))
    print("  auth dir:       %s (exists=%s)" % (cfg.auth_dir, cfg.auth_dir.exists()))
    try:
        print("  service:        %s active=%s" % (cfg.unit, cfg.systemd.is_active()))
    except (ConfigError, DeployError) as exc:
        print("  service:        unknown (%s)" % exc)
    try:
        healthy = cfg.health_probe()
    except Exception:  # noqa: BLE001 - status output must not crash
        healthy = False
    print("  health:         %s ok=%s" % (cfg.health_url, healthy))
    lock = Lock(cfg.lock_path)
    if not cfg.state_dir.exists():
        print("  lock:           state dir absent")
    elif lock.is_held():
        print("  lock:           held by another run")
    else:
        print("  lock:           free")
    snapshots = []
    if cfg.snapshots_dir.is_dir():
        snapshots = sorted(
            name for name in os.listdir(str(cfg.snapshots_dir)) if name.startswith("config.")
        )
    print("  config snapshots: %d in %s" % (len(snapshots), cfg.snapshots_dir))
    return 0


def check_environment(cfg):
    failures = []
    warnings = []

    def record(ok, label, detail="", warn=False):
        if ok:
            _print_line("OK", label, detail)
        elif warn:
            _print_line("WARN", label, detail)
            warnings.append(label)
        else:
            _print_line("FAIL", label, detail)
            failures.append(label)

    print("CLIProxyAPI release-pull deploy environment check")
    record(is_root(), "running as root", "" if is_root() else "required for deploy modes", warn=True)
    record(
        sys.version_info >= (3, 8),
        "python version",
        sys.version.split()[0],
    )
    binary = cfg.binary
    record(binary.is_file(), "live binary exists", str(binary))
    record(
        binary.is_file() and os.access(str(binary), os.X_OK),
        "live binary executable",
        str(binary),
    )
    try:
        probed = normalize_version_text(cfg.probe(binary))
        record(probed is not None, "live binary version", probed or "unstable/unparseable")
    except (ConfigError, DeployError, OSError) as exc:
        record(False, "live binary version", str(exc), warn=True)

    record(cfg.app_dir.is_dir(), "application dir", str(cfg.app_dir))
    record(cfg.config_file.is_file(), "config file exists", str(cfg.config_file))
    state = None
    try:
        state = read_deployed_state(cfg)
    except (ConfigError, RecoveryError) as exc:
        record(False, "deployment state", str(exc))
    if state:
        recorded_sha = state.get("binary_sha256")
        if recorded_sha:
            matches = cfg.binary.is_file() and sha256_file(cfg.binary) == recorded_sha
            record(matches, "live binary matches state", state.get("tag", "unknown"))
        else:
            _print_line("INFO", "live binary matches state", "no digest recorded (baseline)")
    else:
        _print_line("INFO", "deployment state", "not recorded yet; run --baseline")
    if cfg.config_file.is_file() and os.name == "posix":
        st = os.stat(str(cfg.config_file))
        record(
            st.st_uid == 0,
            "config owned by root",
            "uid=%d" % st.st_uid,
            warn=not is_root(),
        )
        record(
            not (st.st_mode & (stat.S_IWGRP | stat.S_IWOTH)),
            "config not group/world writable",
            "mode=%04o" % stat.S_IMODE(st.st_mode),
            warn=True,
        )
    record(cfg.auth_dir.is_dir(), "auth dir exists", str(cfg.auth_dir), warn=True)

    state_parent = cfg.state_dir.parent
    state_ok = cfg.state_dir.is_dir() or os.access(str(state_parent), os.W_OK)
    record(state_ok, "state dir writable", str(cfg.state_dir))

    try:
        exec_start = cfg.systemd.exec_start()
        record(bool(exec_start), "unit ExecStart", exec_start)
        same = bool(exec_start) and os.path.realpath(exec_start) == os.path.realpath(str(binary))
        record(same, "ExecStart matches live binary", "%s vs %s" % (exec_start, binary))
        working_dir = ""
        try:
            working_dir = cfg.systemd.working_directory()
        except (ConfigError, DeployError):
            working_dir = ""
        if working_dir:
            record(
                os.path.realpath(working_dir) == os.path.realpath(str(cfg.app_dir)),
                "WorkingDirectory matches app dir",
                working_dir,
                warn=True,
            )
        else:
            _print_line("WARN", "WorkingDirectory", "not set in the unit")
            warnings.append("WorkingDirectory")
        record(cfg.systemd.is_active(), "service active", cfg.unit)
    except (ConfigError, DeployError) as exc:
        record(False, "systemd unit", str(exc))

    try:
        healthy = cfg.health_probe()
    except Exception:  # noqa: BLE001
        healthy = False
    record(healthy, "health endpoint", cfg.health_url)

    try:
        journal = read_journal(cfg)
    except RecoveryError as exc:
        _print_line("FAIL", "pending journal", str(exc))
        failures.append("pending journal")
        journal = None
    if journal:
        _print_line("WARN", "pending journal", "tag=%s phase=%s" % (journal.get("tag"), journal.get("phase")))
        warnings.append("pending journal")
    elif not cfg.journal_path.exists():
        _print_line("OK", "pending journal", "none")

    backups = [
        name
        for name in (os.listdir(str(cfg.app_dir)) if cfg.app_dir.is_dir() else [])
        if name.startswith(BACKUP_PREFIX)
    ]
    _print_line("INFO", "binary backups", "%d found" % len(backups))
    snapshots = 0
    if cfg.snapshots_dir.is_dir():
        snapshots = len(
            [name for name in os.listdir(str(cfg.snapshots_dir)) if name.startswith("config.")]
        )
    _print_line("INFO", "config snapshots", "%d found" % snapshots)

    print(
        "\n%d checks failed, %d warnings" % (len(failures), len(warnings))
    )
    return 0 if not failures else 1


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def build_parser():
    parser = argparse.ArgumentParser(
        prog="cpap-deploy",
        description=(
            "Pull the newest stable SCNU-PIONEER/CLIProxyAPI GitHub release and\n"
            "atomically deploy its Linux amd64/aarch64 binary to a systemd server."
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "modes:\n"
            "  (none)          inspect local deployment state, no network\n"
            "  --check         validate the host, unit, binary, config, health\n"
            "  --dry-run       show the release that would be deployed\n"
            "  --latest        deploy the newest deployable stable release\n"
            "  --tag vX.Y.Z    deploy one explicit stable release (no downgrade)\n"
            "  --baseline      record the running version without upgrading\n"
            "  --abort-journal discard a pending journal after manual recovery\n"
        ),
    )
    parser.add_argument("--binary", default=DEFAULT_BINARY, help="live binary path")
    parser.add_argument("--app-dir", default=DEFAULT_APP_DIR, help="application working directory")
    parser.add_argument("--config", dest="config_file", default=None, help="config.yaml path")
    parser.add_argument("--auth-dir", default=DEFAULT_AUTH_DIR, help="OAuth/auth directory")
    parser.add_argument("--unit", default=DEFAULT_UNIT, help="systemd unit name")
    parser.add_argument("--port", type=int, default=DEFAULT_PORT, help="local API port")
    parser.add_argument("--health-url", default=None, help="override the health check URL")
    parser.add_argument("--state-dir", default=DEFAULT_STATE_DIR, help="deployer state directory")
    parser.add_argument("--repo", default=DEFAULT_REPO, help="GitHub repository (owner/name)")
    parser.add_argument("--github-base", default=DEFAULT_GITHUB_BASE, help="GitHub base URL")
    parser.add_argument("--arch", default="", help="override architecture (amd64/aarch64)")
    parser.add_argument("--keep-backups", type=int, default=DEFAULT_KEEP_BACKUPS, help="backups to keep")
    parser.add_argument("--health-attempts", type=int, default=20, help="health check attempts")
    parser.add_argument("--health-interval", type=float, default=2.0, help="seconds between health checks")
    parser.add_argument("--http-timeout", type=float, default=20.0, help="HTTP timeout in seconds")
    parser.add_argument("--tag", default=None, help="deploy exactly this stable release tag")
    parser.add_argument("--retry", action="store_true", help="retry the last failed release")
    parser.add_argument(
        "--allow-major",
        action="store_true",
        help="opt in to a major-version upgrade (otherwise major bumps are skipped)",
    )
    parser.add_argument("--baseline-version", default=None, help="version to record with --baseline")
    parser.add_argument(
        "--allow-non-root",
        action="store_true",
        help="skip the root check (dry-run previews and offline tests only)",
    )
    parser.add_argument("--debug", action="store_true", help="print tracebacks on unexpected errors")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--check", action="store_true", help="validate the environment")
    mode.add_argument("--dry-run", action="store_true", help="preview without changing anything")
    mode.add_argument("--latest", action="store_true", help="deploy the newest release")
    mode.add_argument("--baseline", action="store_true", help="record the running version")
    mode.add_argument("--abort-journal", action="store_true", help="discard a pending journal")
    parser.add_argument("--version", action="version", version="cpap-deploy 1.0")
    return parser


def config_from_args(args):
    from pathlib import Path

    if args.port < 1 or args.port > 65535:
        raise ConfigError("--port must be between 1 and 65535")
    if args.keep_backups < 1:
        raise ConfigError("--keep-backups must be at least 1")
    if args.health_attempts < 1:
        raise ConfigError("--health-attempts must be at least 1")
    if args.health_interval <= 0 or args.http_timeout <= 0:
        raise ConfigError("timeouts must be positive")
    app_dir = Path(args.app_dir).expanduser()
    config_file = Path(args.config_file).expanduser() if args.config_file else app_dir / "config.yaml"
    health_url = args.health_url or ("http://127.0.0.1:%d/healthz" % args.port)
    parsed = urllib.parse.urlsplit(health_url)
    if parsed.scheme not in ("http", "https") or not parsed.netloc:
        raise ConfigError("--health-url must be an http(s) URL")
    return Config(
        binary=Path(args.binary).expanduser(),
        app_dir=app_dir,
        config_file=config_file,
        auth_dir=Path(args.auth_dir).expanduser(),
        unit=args.unit,
        health_url=health_url,
        state_dir=Path(args.state_dir).expanduser(),
        repo=args.repo,
        github_base=args.github_base,
        arch=args.arch,
        keep_backups=args.keep_backups,
        health_attempts=args.health_attempts,
        health_interval=args.health_interval,
        http_timeout=args.http_timeout,
        allow_non_root=args.allow_non_root,
    )


def validate_args(args):
    """Reject contradictory flag combinations before any network or state access."""
    if args.tag and args.latest:
        raise ConfigError("--tag and --latest are mutually exclusive")
    if args.tag:
        conflicts = []
        if args.check:
            conflicts.append("--check")
        if args.baseline:
            conflicts.append("--baseline")
        if args.abort_journal:
            conflicts.append("--abort-journal")
        if conflicts:
            raise ConfigError(
                "--tag cannot be combined with %s; use --tag --dry-run to preview"
                % ", ".join(conflicts)
            )
    if args.retry and not (args.latest or args.tag):
        raise ConfigError("--retry requires --latest or --tag")
    if args.allow_major and not (args.latest or args.tag or args.dry_run):
        raise ConfigError("--allow-major requires --latest, --tag, or --dry-run")
    if args.baseline_version and not args.baseline:
        raise ConfigError("--baseline-version requires --baseline")


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        validate_args(args)
        cfg = config_from_args(args)
        if args.check:
            return check_environment(cfg)
        if args.baseline:
            return record_baseline(cfg, args.baseline_version)
        if args.abort_journal:
            return abort_journal(cfg)
        if args.latest or args.tag or args.dry_run:
            return run_deploy(
                cfg,
                explicit_tag=args.tag,
                retry=args.retry,
                dry_run=args.dry_run,
                allow_major=args.allow_major,
            )
        return inspect_status(cfg)
    except ConfigError as exc:
        print("error: %s" % exc, file=sys.stderr)
        return 2
    except PolicyError as exc:
        print("refused: %s" % exc, file=sys.stderr)
        return 3
    except DeployError as exc:
        print("error: %s" % exc, file=sys.stderr)
        return 1
    except RecoveryError as exc:
        print("error: %s" % exc, file=sys.stderr)
        return 5
    except FetchError as exc:
        print("error: cannot reach GitHub: %s" % exc, file=sys.stderr)
        return 1
    except ChecksumError as exc:
        print("error: %s" % exc, file=sys.stderr)
        return 5
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        return 1
    except Exception as exc:  # noqa: BLE001
        if args.debug:
            raise
        print("unexpected error: %s" % exc, file=sys.stderr)
        return 1
    finally:
        if "cfg" in locals():
            cfg.log.close()


if __name__ == "__main__":
    sys.exit(main())
