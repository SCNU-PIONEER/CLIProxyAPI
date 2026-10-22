"""Tests for the CPAP release-pull installer and its systemd units.

The installer is a shell script, so the execution tests drive it with a fake
root prefix and fake ``python3``/``systemctl`` commands. Nothing outside the
test's temporary directory is written, which keeps the suite safe on CI hosts
that do have a real systemd and a real ``/root/cliproxyapi``.
"""

import os
import re
import shutil
import stat
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
INSTALL = HERE / "install.sh"
SERVICE = HERE / "cpap-deploy.service"
TIMER = HERE / "cpap-deploy.timer"
MARKER = "# Managed by CLIProxyAPI deployment/cpap-gateway/install.sh - do not edit."


def _bash():
    return shutil.which("bash")


def _to_posix(path):
    """Return a path the Git Bash (or Linux) shell can use."""
    value = str(path)
    if os.name != "nt":
        return value
    cygpath = shutil.which("cygpath")
    if not cygpath:
        return value.replace("\\", "/")
    completed = subprocess.run(
        [cygpath, "-u", value],
        check=True,
        capture_output=True,
        text=True,
        encoding="utf-8",
    )
    return completed.stdout.strip()


def _posix_path_env():
    """Return PATH in the form the shell under test understands."""
    value = os.environ.get("PATH", "")
    if os.name != "nt":
        return value
    cygpath = shutil.which("cygpath")
    if not cygpath:
        return value.replace("\\", "/").replace(";", ":")
    completed = subprocess.run(
        [cygpath, "-p", value],
        check=True,
        capture_output=True,
        text=True,
        encoding="utf-8",
    )
    return completed.stdout.strip()


class UnitFileTests(unittest.TestCase):
    """The packaged units must stay boring and match the installer contract."""

    def test_service_runs_only_the_fixed_latest_wrapper(self):
        text = SERVICE.read_text(encoding="utf-8")
        self.assertIn(MARKER, text)
        self.assertIn("Type=oneshot\n", text)
        self.assertIn("User=root\n", text)
        self.assertIn("ExecStart=/usr/local/bin/cpap-deploy --latest\n", text)
        self.assertIn("TimeoutStartSec=20min\n", text)
        self.assertNotIn("--allow-non-root", text)
        self.assertNotIn("--dry-run", text)
        self.assertNotIn("--check", text)
        self.assertNotIn("[Install]", text)

    def test_service_does_not_touch_the_application(self):
        text = SERVICE.read_text(encoding="utf-8")
        self.assertNotIn("cliproxyapi.service", text)
        self.assertNotIn("/root/cliproxyapi/config.yaml", text)
        self.assertNotIn("/root/.cli-proxy-api", text)
        self.assertNotIn("/root/cliproxyapi/logs", text)
        self.assertNotIn("systemctl", text.replace("systemd", ""))
        self.assertNotIn("Restart=always", text)

    def test_timer_checks_every_five_minutes(self):
        text = TIMER.read_text(encoding="utf-8")
        self.assertIn(MARKER, text)
        self.assertIn("OnActiveSec=5min\n", text)
        self.assertIn("OnUnitActiveSec=5min\n", text)
        self.assertIn("RandomizedDelaySec=60\n", text)
        self.assertIn("Unit=cpap-deploy.service\n", text)
        self.assertIn("WantedBy=timers.target\n", text)
        self.assertNotIn("Persistent=true", text)
        self.assertNotIn("OnCalendar=", text)

    def test_installer_never_installs_or_runs_unsafe_modes(self):
        text = INSTALL.read_text(encoding="utf-8")
        self.assertNotIn("--allow-non-root", text)
        self.assertNotIn("systemctl restart", text)
        self.assertNotIn("systemctl start cpap-deploy", text)
        self.assertNotIn("curl", text)
        self.assertNotIn("wget", text)


@unittest.skipUnless(_bash(), "bash is required to exercise the installer")
class InstallerExecutionTests(unittest.TestCase):
    """Run the installer against a disposable fake root."""

    @classmethod
    def setUpClass(cls):
        cls.bash = _bash()
        cls.install_posix = _to_posix(INSTALL)
        cls.repo_windows = str(HERE)
        cls.path_env = _posix_path_env()

    def setUp(self):
        self.root = Path(tempfile.mkdtemp(prefix="cpap-install-test-"))
        self.root_posix = _to_posix(self.root)
        self.app = self.root / "root" / "cliproxyapi"
        self.auth = self.root / "root" / ".cli-proxy-api"
        self.state = self.root / "root" / "cliproxyapi.deploy"
        self.bin_dir = self.root / "usr" / "local" / "bin"
        self.lib_dir = self.root / "usr" / "local" / "lib" / "cpap-deploy"
        self.unit_dir = self.root / "etc" / "systemd" / "system"
        self.fakebin = self.root / "fakebin"
        for directory in (self.app / "logs", self.auth, self.bin_dir, self.unit_dir, self.fakebin):
            directory.mkdir(parents=True, exist_ok=True)

        self.binary = self.app / "cli-proxy-api"
        self.binary.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        os.chmod(self.binary, 0o755)
        self.config = self.app / "config.yaml"
        self.config.write_text("port: 8317\n", encoding="utf-8")
        (self.app / "logs" / "server.log").write_text("keep me\n", encoding="utf-8")
        (self.auth / "account.json").write_text('{"keep": true}\n', encoding="utf-8")
        self.app_unit = self.unit_dir / "cliproxyapi.service"
        self.app_unit.write_text(
            "[Service]\nExecStart=/root/cliproxyapi/cli-proxy-api\n", encoding="utf-8"
        )

        self.events = self.root / "events.log"
        self.py_log = self.root / "python.log"
        self.sc_log = self.root / "systemctl.log"
        self.sc_state = self.root / "systemctl.enabled"
        self.sc_fail = self.root / "systemctl.fail"
        self.py_fail = self.root / "python.fail"
        for path in (self.events, self.py_log, self.sc_log):
            path.write_text("", encoding="utf-8")

        self._write_fake_python()
        self._write_fake_systemctl()
        os.chmod(self.fakebin / "python3", 0o755)
        os.chmod(self.fakebin / "systemctl", 0o755)

        self.env = os.environ.copy()
        self.env.update(
            {
                "CPAP_DEPLOY_TEST_MODE": "1",
                "CPAP_DEPLOY_TEST_ROOT": self.root_posix,
                "PATH": _to_posix(self.fakebin) + ":" + self.path_env,
                "FAKE_EVENTS_LOG": _to_posix(self.events),
                "FAKE_PYTHON_LOG": _to_posix(self.py_log),
                "FAKE_SYSTEMCTL_LOG": _to_posix(self.sc_log),
                "FAKE_SYSTEMCTL_STATE": _to_posix(self.sc_state),
                "FAKE_SYSTEMCTL_FAIL": _to_posix(self.sc_fail),
                "FAKE_PYTHON_FAIL": _to_posix(self.py_fail),
            }
        )
        self.env.pop("CPAP_DEPLOY_TEST_MODE_OVERRIDE", None)

    def tearDown(self):
        shutil.rmtree(self.root, ignore_errors=True)

    # -- harness ---------------------------------------------------------

    def _write_fake_python(self):
        (self.fakebin / "python3").write_text(
            textwrap.dedent(
                """\
                #!/bin/sh
                printf 'python3 %s\\n' "$*" >> "$FAKE_EVENTS_LOG"
                printf '%s\\n' "$*" >> "$FAKE_PYTHON_LOG"
                for arg in "$@"; do
                    if [ "$arg" = "-c" ]; then
                        exit 0
                    fi
                    if [ "$arg" = "--version" ]; then
                        printf 'cpap-deploy 1.0\\n'
                        exit 0
                    fi
                done
                if [ -f "$FAKE_PYTHON_FAIL" ]; then
                    while IFS= read -r mode; do
                        [ -n "$mode" ] || continue
                        case " $* " in
                            *" $mode "*) printf 'fake python failure: %s\\n' "$mode" >&2; exit 1 ;;
                        esac
                    done < "$FAKE_PYTHON_FAIL"
                fi
                case " $* " in
                    *" --check "*)
                        printf 'OK    ExecStart matches live binary\\n'
                        printf 'OK    WorkingDirectory matches app dir\\n'
                        ;;
                    *" --baseline "*)
                        printf 'baseline recorded or already present\\n'
                        ;;
                    *" --dry-run "*)
                        printf 'plan: no action\\n'
                        ;;
                    *" --latest "*)
                        printf 'unexpected --latest invocation\\n' >&2
                        exit 9
                        ;;
                esac
                exit 0
                """
            ),
            encoding="utf-8",
        )

    def _write_fake_systemctl(self):
        (self.fakebin / "systemctl").write_text(
            textwrap.dedent(
                """\
                #!/bin/sh
                printf 'systemctl %s\\n' "$*" >> "$FAKE_EVENTS_LOG"
                printf '%s\\n' "$*" >> "$FAKE_SYSTEMCTL_LOG"
                if [ -f "$FAKE_SYSTEMCTL_FAIL" ]; then
                    while IFS= read -r verb; do
                        [ -n "$verb" ] || continue
                        if [ "$1" = "$verb" ]; then
                            printf 'fake systemctl failure: %s\\n' "$verb" >&2
                            exit 1
                        fi
                    done < "$FAKE_SYSTEMCTL_FAIL"
                fi
                case "$1" in
                    enable) : > "$FAKE_SYSTEMCTL_STATE"; exit 0 ;;
                    disable) rm -f "$FAKE_SYSTEMCTL_STATE"; exit 0 ;;
                    is-enabled) [ -f "$FAKE_SYSTEMCTL_STATE" ] && exit 0 || exit 1 ;;
                    is-active) [ -f "$FAKE_SYSTEMCTL_STATE" ] && exit 0 || exit 1 ;;
                    daemon-reload) exit 0 ;;
                    start|stop|restart|reload)
                        printf 'unexpected mutating systemctl verb: %s\\n' "$*" >&2
                        exit 9
                        ;;
                esac
                exit 0
                """
            ),
            encoding="utf-8",
        )

    def run_installer(self, *args, env_extra=None):
        env = self.env.copy()
        if env_extra:
            env.update(env_extra)
        return subprocess.run(
            [self.bash, self.install_posix, *args],
            cwd=self.repo_windows,
            env=env,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=120,
        )

    def clear_logs(self):
        for path in (self.events, self.py_log, self.sc_log):
            path.write_text("", encoding="utf-8")

    def python_calls(self):
        return [
            line for line in self.py_log.read_text(encoding="utf-8").splitlines() if line
        ]

    def systemctl_calls(self):
        return [
            line for line in self.sc_log.read_text(encoding="utf-8").splitlines() if line
        ]

    def event_lines(self):
        return [
            line for line in self.events.read_text(encoding="utf-8").splitlines() if line
        ]

    def assert_ok(self, completed):
        self.assertEqual(
            completed.returncode,
            0,
            "installer failed\nstdout:\n%s\nstderr:\n%s"
            % (completed.stdout, completed.stderr),
        )

    def install_paths(self):
        return {
            "updater": self.lib_dir / "cpap_deploy.py",
            "wrapper": self.bin_dir / "cpap-deploy",
            "service": self.unit_dir / "cpap-deploy.service",
            "timer": self.unit_dir / "cpap-deploy.timer",
        }

    def assert_install_absent(self):
        for path in self.install_paths().values():
            self.assertFalse(path.exists(), "unexpected installed file: %s" % path)

    def assert_application_untouched(self):
        self.assertEqual(self.binary.read_text(encoding="utf-8"), "#!/bin/sh\nexit 0\n")
        self.assertEqual(self.config.read_text(encoding="utf-8"), "port: 8317\n")
        self.assertEqual(
            (self.app / "logs" / "server.log").read_text(encoding="utf-8"), "keep me\n"
        )
        self.assertEqual(
            (self.auth / "account.json").read_text(encoding="utf-8"), '{"keep": true}\n'
        )
        self.assertEqual(
            self.app_unit.read_text(encoding="utf-8"),
            "[Service]\nExecStart=/root/cliproxyapi/cli-proxy-api\n",
        )

    # -- help and argument validation -----------------------------------

    def test_help_mentions_enable_and_baseline(self):
        completed = self.run_installer("--help")
        self.assert_ok(completed)
        self.assertIn("--enable", completed.stdout)
        self.assertIn("--baseline-version", completed.stdout)
        self.assertIn("--latest", completed.stdout)
        self.assert_install_absent()

    def test_unknown_option_is_usage_error(self):
        completed = self.run_installer("--bogus")
        self.assertEqual(completed.returncode, 2)
        self.assertIn("unknown argument", completed.stderr)
        self.assert_install_absent()

    def test_invalid_baseline_version_is_usage_error(self):
        completed = self.run_installer("--baseline-version", "8.0")
        self.assertEqual(completed.returncode, 2)
        self.assertIn("vX.Y.Z", completed.stderr)
        self.assert_install_absent()

    def test_baseline_version_forwards_to_the_updater(self):
        completed = self.run_installer("--baseline-version", "8.0.38")
        self.assert_ok(completed)
        calls = self.python_calls()
        self.assertTrue(
            any("--baseline --baseline-version 8.0.38" in call for call in calls),
            calls,
        )

    def test_test_root_requires_the_internal_mode(self):
        completed = self.run_installer(
            env_extra={"CPAP_DEPLOY_TEST_MODE": "0"}
        )
        self.assertEqual(completed.returncode, 1)
        self.assertIn("CPAP_DEPLOY_TEST_ROOT", completed.stderr)
        self.assert_install_absent()

    # -- safe install ---------------------------------------------------

    def test_default_install_is_disabled_and_runs_all_previews(self):
        completed = self.run_installer()
        self.assert_ok(completed)

        paths = self.install_paths()
        for path in paths.values():
            self.assertTrue(path.is_file(), path)

        wrapper = paths["wrapper"].read_text(encoding="utf-8")
        self.assertIn(MARKER, wrapper)
        self.assertIn("exec ", wrapper)
        self.assertIn('"$@"', wrapper)
        for argument in (
            "--binary",
            "--app-dir",
            "--config",
            "--auth-dir",
            "--unit",
            "--port",
            "--state-dir",
            "--repo",
            "--keep-backups",
        ):
            self.assertIn("'%s'" % argument, wrapper)
        self.assertIn("cli-proxy-api", wrapper)
        self.assertNotIn("--allow-non-root", wrapper)
        self.assertNotIn("--latest", wrapper)

        calls = self.python_calls()
        self.assertEqual(sum("--check" in call for call in calls), 2, calls)
        self.assertEqual(sum("--baseline" in call for call in calls), 1, calls)
        self.assertEqual(sum("--dry-run" in call for call in calls), 1, calls)
        self.assertFalse(any("--latest" in call for call in calls), calls)

        system_calls = self.systemctl_calls()
        self.assertIn("daemon-reload", system_calls)
        mutations = [
            call
            for call in system_calls
            if re.match(r"^(enable|disable|start|stop|restart)\b", call)
        ]
        self.assertEqual(mutations, [], system_calls)
        self.assertFalse(self.sc_state.exists())

        self.assert_application_untouched()
        self.assertIn("not enabled", completed.stdout)

        if os.name == "posix":
            self.assertEqual(stat.S_IMODE(paths["updater"].stat().st_mode), 0o644)
            self.assertEqual(stat.S_IMODE(paths["wrapper"].stat().st_mode), 0o755)
            self.assertEqual(stat.S_IMODE(paths["service"].stat().st_mode), 0o644)
            self.assertEqual(stat.S_IMODE(paths["timer"].stat().st_mode), 0o644)
            self.assertEqual(stat.S_IMODE(self.lib_dir.stat().st_mode), 0o755)
            self.assertEqual(stat.S_IMODE(self.state.stat().st_mode), 0o700)

    def test_enable_arms_the_timer_only_after_the_preview(self):
        completed = self.run_installer("--enable")
        self.assert_ok(completed)
        self.assertTrue(self.sc_state.exists())

        events = self.event_lines()
        preview_index = next(
            index for index, line in enumerate(events) if line.startswith("python3") and "--dry-run" in line
        )
        enable_index = next(
            index
            for index, line in enumerate(events)
            if line.startswith("systemctl enable --now cpap-deploy.timer")
        )
        self.assertLess(preview_index, enable_index, events)
        self.assertIn("is-enabled --quiet cpap-deploy.timer", self.systemctl_calls())
        self.assertIn("is-active --quiet cpap-deploy.timer", self.systemctl_calls())
        self.assertIn("enabled and active", completed.stdout)

    def test_enable_refuses_when_the_preview_fails(self):
        self.py_fail.write_bytes(b"--dry-run\n")
        completed = self.run_installer("--enable")
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("refusing to enable", completed.stderr)
        self.assertFalse(self.sc_state.exists())
        system_calls = self.systemctl_calls()
        self.assertFalse(any("enable" in call for call in system_calls), system_calls)
        self.assertFalse(any("start" in call for call in system_calls), system_calls)

    def test_default_install_tolerates_a_failed_preview_but_stays_disabled(self):
        self.py_fail.write_bytes(b"--dry-run\n")
        completed = self.run_installer()
        self.assert_ok(completed)
        self.assertIn("warning", completed.stderr.lower())
        self.assertFalse(self.sc_state.exists())
        mutations = [
            call
            for call in self.systemctl_calls()
            if re.match(r"^(enable|disable|start|stop|restart)\b", call)
        ]
        self.assertEqual(mutations, [], self.systemctl_calls())

    def test_source_check_failure_changes_nothing(self):
        self.py_fail.write_bytes(b"--check\n")
        completed = self.run_installer()
        self.assertEqual(completed.returncode, 1)
        self.assertIn("nothing was changed", completed.stderr)
        self.assert_install_absent()
        self.assertFalse(self.state.exists())
        self.assert_application_untouched()

    def test_working_directory_warning_fails_the_install(self):
        fake = self.fakebin / "python3"
        text = fake.read_text(encoding="utf-8").replace(
            "printf 'OK    WorkingDirectory matches app dir\\n'",
            "printf 'WARN  WorkingDirectory is not set\\n'",
        )
        fake.write_text(text, encoding="utf-8")
        completed = self.run_installer()
        self.assertEqual(completed.returncode, 1)
        self.assertIn("unit path verification did not pass", completed.stderr)
        self.assert_install_absent()

    # -- ownership and upgrade safety -----------------------------------

    def test_existing_state_journal_and_failure_are_preserved(self):
        self.state.mkdir(parents=True)
        os.chmod(self.state, 0o700)
        deployed = self.state / "deployed.json"
        journal = self.state / "journal.json"
        failure = self.state / "last_failure.json"
        deployed.write_text('{"tag": "v8.0.38"}\n', encoding="utf-8")
        journal.write_text('{"phase": "health-check"}\n', encoding="utf-8")
        failure.write_text('{"tag": "v8.0.37"}\n', encoding="utf-8")

        completed = self.run_installer("--enable")
        self.assert_ok(completed)
        self.assertEqual(deployed.read_text(encoding="utf-8"), '{"tag": "v8.0.38"}\n')
        self.assertEqual(journal.read_text(encoding="utf-8"), '{"phase": "health-check"}\n')
        self.assertEqual(failure.read_text(encoding="utf-8"), '{"tag": "v8.0.37"}\n')
        calls = self.python_calls()
        self.assertFalse(any("--abort-journal" in call for call in calls), calls)

    def test_reinstall_without_enable_preserves_an_enabled_timer(self):
        first = self.run_installer("--enable")
        self.assert_ok(first)
        self.assertTrue(self.sc_state.exists())
        self.clear_logs()

        second = self.run_installer()
        self.assert_ok(second)
        self.assertTrue(self.sc_state.exists(), "installer must not disable the timer")
        system_calls = self.systemctl_calls()
        mutations = [
            call
            for call in system_calls
            if re.match(r"^(enable|disable|start|stop|restart)\b", call)
        ]
        self.assertEqual(mutations, [], system_calls)
        self.assertIn("left untouched", second.stdout)
        self.assertNotIn("installed but not enabled", second.stdout)
        self.assertIn("already enabled", second.stdout)

    def test_preview_failure_does_not_disable_an_already_enabled_timer(self):
        first = self.run_installer("--enable")
        self.assert_ok(first)
        self.py_fail.write_bytes(b"--dry-run\n")
        self.clear_logs()

        second = self.run_installer("--enable")
        self.assertNotEqual(second.returncode, 0)
        self.assertTrue(self.sc_state.exists(), "existing timer state must be preserved")
        self.assertFalse(
            any("disable" in call for call in self.systemctl_calls()),
            self.systemctl_calls(),
        )

    def test_systemctl_enable_failure_leaves_a_new_install_disabled(self):
        self.sc_fail.write_bytes(b"enable\n")
        completed = self.run_installer("--enable")
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("enable --now", completed.stderr)
        self.assertFalse(self.sc_state.exists())

    def test_systemctl_enable_failure_preserves_an_enabled_timer(self):
        first = self.run_installer("--enable")
        self.assert_ok(first)
        self.sc_fail.write_bytes(b"enable\n")
        self.clear_logs()

        second = self.run_installer("--enable")
        self.assertNotEqual(second.returncode, 0)
        self.assertTrue(self.sc_state.exists(), "existing timer state must be preserved")
        self.assertFalse(
            any(call.startswith("disable") for call in self.systemctl_calls()),
            self.systemctl_calls(),
        )

    def test_foreign_wrapper_is_not_overwritten(self):
        foreign = self.bin_dir / "cpap-deploy"
        foreign.write_text("#!/bin/sh\necho foreign\n", encoding="utf-8")
        os.chmod(foreign, 0o755)
        completed = self.run_installer()
        self.assertEqual(completed.returncode, 1)
        self.assertIn("unmanaged wrapper", completed.stderr)
        self.assertEqual(foreign.read_text(encoding="utf-8"), "#!/bin/sh\necho foreign\n")
        self.assertFalse((self.lib_dir / "cpap_deploy.py").exists())

    def test_foreign_unit_is_not_overwritten(self):
        foreign = self.unit_dir / "cpap-deploy.service"
        foreign.write_text("[Service]\nExecStart=/bin/true\n", encoding="utf-8")
        completed = self.run_installer()
        self.assertEqual(completed.returncode, 1)
        self.assertIn("unmanaged unit", completed.stderr)
        self.assertEqual(foreign.read_text(encoding="utf-8"), "[Service]\nExecStart=/bin/true\n")
        self.assertFalse((self.lib_dir / "cpap_deploy.py").exists())

    def test_foreign_updater_is_not_overwritten(self):
        self.lib_dir.mkdir(parents=True)
        foreign = self.lib_dir / "cpap_deploy.py"
        foreign.write_text("# foreign updater\n", encoding="utf-8")
        completed = self.run_installer()
        self.assertEqual(completed.returncode, 1)
        self.assertIn("unmanaged updater", completed.stderr)
        self.assertEqual(foreign.read_text(encoding="utf-8"), "# foreign updater\n")
        self.assertFalse((self.bin_dir / "cpap-deploy").exists())

    def test_symlinked_target_is_refused(self):
        victim = self.root / "victim"
        victim.write_text("do not touch\n", encoding="utf-8")
        link = self.bin_dir / "cpap-deploy"
        try:
            os.symlink(victim, link)
        except (OSError, NotImplementedError) as error:
            self.skipTest("cannot create symlinks here: %s" % error)

        completed = self.run_installer()
        self.assertEqual(completed.returncode, 1)
        self.assertIn("symlink", completed.stderr)
        self.assertEqual(victim.read_text(encoding="utf-8"), "do not touch\n")
        self.assertFalse((self.lib_dir / "cpap_deploy.py").exists())

    def test_installed_wrapper_passes_extra_arguments_through(self):
        completed = self.run_installer()
        self.assert_ok(completed)
        wrapper = self.install_paths()["wrapper"]
        self.clear_logs()

        ran = subprocess.run(
            [self.bash, _to_posix(wrapper), "--tag", "v9.9.9"],
            cwd=self.repo_windows,
            env=self.env,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=60,
        )
        self.assertEqual(ran.returncode, 0, ran.stderr)
        calls = self.python_calls()
        self.assertTrue(calls, calls)
        self.assertIn("--tag v9.9.9", calls[-1])
        self.assertIn("--binary", calls[-1])
        self.assertIn("--latest", self.install_paths()["service"].read_text(encoding="utf-8"))
        self.assertNotIn("--latest", calls[-1])


if __name__ == "__main__":
    unittest.main()
