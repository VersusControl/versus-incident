"""Bootstrap recovery behavior for standalone and caller-configured deployments."""

import importlib.util
import io
import os
import stat
from contextlib import redirect_stdout
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("bootstrap", Path(__file__).with_name("bootstrap.py"))
bootstrap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bootstrap)


class BootstrapRecoveryTest(unittest.TestCase):
    def test_stale_key_uses_standalone_guidance_without_restarting(self):
        with tempfile.TemporaryDirectory() as directory:
            key_file = Path(directory) / "key"
            key_file.write_text("secret-key")
            output = io.StringIO()
            with patch.object(bootstrap, "KEY_FILE", str(key_file)), patch.object(
                bootstrap, "wait_ready", return_value={"setupCompleted": True}
            ), patch.object(bootstrap, "call", return_value=(401, {"key": "secret-body"})), patch.dict(
                os.environ, {"SIGNOZ_API_KEY": "", "SIGNOZ_RESET_COMMAND": ""}
            ), redirect_stdout(output):
                self.assertEqual(bootstrap.main(), 1)
            self.assertIn("reset the SigNoz installation", output.getvalue())
            self.assertNotIn("harness-run", output.getvalue())
            self.assertNotIn("secret", output.getvalue())

    def test_stale_key_uses_supplied_hint(self):
        with tempfile.TemporaryDirectory() as directory:
            key_file = Path(directory) / "key"
            key_file.write_text("secret-key")
            output = io.StringIO()
            with patch.object(bootstrap, "KEY_FILE", str(key_file)), patch.object(
                bootstrap, "wait_ready", return_value={"setupCompleted": True}
            ), patch.object(bootstrap, "call", return_value=(403, {})), patch.dict(
                os.environ, {"SIGNOZ_API_KEY": "", "SIGNOZ_RESET_COMMAND": "harness-run/harness.sh reset"}
            ), redirect_stdout(output):
                self.assertEqual(bootstrap.main(), 1)
            self.assertIn("harness-run/harness.sh reset", output.getvalue())
            self.assertNotIn("secret-key", output.getvalue())

    def test_unverified_key_preserves_state(self):
        for status, body in ((0, "secret-timeout"), (500, {"key": "secret-body"}),
                             (404, "secret-body"), (200, "secret-body")):
            with self.subTest(status=status, body_type=type(body).__name__), tempfile.TemporaryDirectory() as directory:
                key_file = Path(directory) / "key"
                key_file.write_text("secret-key")
                output = io.StringIO()
                with patch.object(bootstrap, "KEY_FILE", str(key_file)), patch.object(
                    bootstrap, "wait_ready", return_value={"setupCompleted": True}
                ), patch.object(bootstrap, "call", return_value=(status, body)), patch.dict(
                    os.environ, {"SIGNOZ_API_KEY": "", "SIGNOZ_RESET_COMMAND": "harness-run/harness.sh reset"}
                ), redirect_stdout(output):
                    self.assertEqual(bootstrap.main(), 1)
                self.assertIn("retry/check SigNoz health or API compatibility; preserve state", output.getvalue())
                self.assertNotIn("reset", output.getvalue())
                self.assertNotIn("secret", output.getvalue())
                self.assertEqual(key_file.read_text(), "secret-key")

    def test_direct_timeout_preserves_state(self):
        with tempfile.TemporaryDirectory() as directory:
            key_file = Path(directory) / "key"
            key_file.write_text("secret-key")
            output = io.StringIO()
            with patch.object(bootstrap, "KEY_FILE", str(key_file)), patch.object(
                bootstrap, "wait_ready", return_value={"setupCompleted": True}
            ), patch.object(bootstrap.urllib.request, "urlopen", side_effect=TimeoutError("secret-timeout")), patch.dict(
                os.environ, {"SIGNOZ_API_KEY": "", "SIGNOZ_RESET_COMMAND": "harness-run/harness.sh reset"}
            ), redirect_stdout(output):
                self.assertEqual(bootstrap.main(), 1)
            self.assertIn("HTTP 0", output.getvalue())
            self.assertIn("preserve state", output.getvalue())
            self.assertNotIn("reset", output.getvalue())
            self.assertNotIn("secret", output.getvalue())
            self.assertEqual(key_file.read_text(), "secret-key")

    def test_initialized_without_key_does_not_register(self):
        with tempfile.TemporaryDirectory() as directory:
            output = io.StringIO()
            with patch.object(bootstrap, "KEY_FILE", str(Path(directory) / "missing")), patch.object(
                bootstrap, "wait_ready", return_value={"setupCompleted": True}
            ), patch.object(bootstrap, "register") as register, patch.dict(
                os.environ, {"SIGNOZ_API_KEY": "", "SIGNOZ_RESET_COMMAND": ""}
            ), redirect_stdout(output):
                self.assertEqual(bootstrap.main(), 1)
            register.assert_not_called()
            self.assertIn("no saved API key", output.getvalue())
            self.assertIn("reset the SigNoz installation", output.getvalue())
            self.assertNotIn("harness-run", output.getvalue())

    def test_valid_saved_key_succeeds(self):
        with tempfile.TemporaryDirectory() as directory:
            key_file = Path(directory) / "key"
            key_file.write_text("valid-key")
            output = io.StringIO()
            with patch.object(bootstrap, "KEY_FILE", str(key_file)), patch.object(
                bootstrap, "wait_ready", return_value={"setupCompleted": True}
            ), patch.object(bootstrap, "call", return_value=(200, {})) as call, patch.dict(
                os.environ, {"SIGNOZ_API_KEY": ""}
            ), redirect_stdout(output):
                self.assertEqual(bootstrap.main(), 0)
            call.assert_called_once_with("/api/v1/service_accounts/me", api_key="valid-key")
            self.assertNotIn("valid-key", output.getvalue())

    def test_fresh_boot_writes_key_and_succeeds(self):
        with tempfile.TemporaryDirectory() as directory:
            key_file = Path(directory) / "bootstrap" / "key"
            output = io.StringIO()
            real_mkstemp = tempfile.mkstemp
            real_replace = os.replace

            def check_creation(*args, **kwargs):
                fd, temporary = real_mkstemp(*args, **kwargs)
                self.assertEqual(stat.S_IMODE(os.fstat(fd).st_mode), 0o600)
                return fd, temporary

            def check_replacement(temporary, destination):
                self.assertEqual(stat.S_IMODE(os.stat(temporary).st_mode), 0o600)
                self.assertFalse(key_file.exists())
                return real_replace(temporary, destination)

            old_umask = os.umask(0o000)
            try:
                with patch.object(bootstrap.tempfile, "mkstemp", side_effect=check_creation), patch.object(
                    bootstrap.os, "replace", side_effect=check_replacement
                ), patch.object(bootstrap, "KEY_FILE", str(key_file)), patch.object(
                    bootstrap, "wait_ready", return_value={"setupCompleted": False}
                ), patch.object(bootstrap, "register", return_value="org") as register, patch.object(
                    bootstrap, "login", return_value="token"
                ), patch.object(bootstrap, "service_account", return_value="account"), patch.object(
                    bootstrap, "grant_role"
                ), patch.object(bootstrap, "create_key", return_value="minted-key"), patch.dict(
                    os.environ, {"SIGNOZ_API_KEY": ""}
                ), redirect_stdout(output):
                    self.assertEqual(bootstrap.main(), 0)
            finally:
                os.umask(old_umask)
            register.assert_called_once_with()
            self.assertEqual(key_file.read_text(), "minted-key")
            self.assertEqual(stat.S_IMODE(key_file.stat().st_mode), 0o600)
            self.assertEqual(list(key_file.parent.iterdir()), [key_file])
            self.assertNotIn("minted-key", output.getvalue())

    def test_empty_key_replacement_retains_owner(self):
        with tempfile.TemporaryDirectory() as directory:
            key_file = Path(directory) / "key"
            key_file.touch()
            previous = key_file.stat()
            with patch.object(bootstrap, "KEY_FILE", str(key_file)), patch.object(
                bootstrap, "wait_ready", return_value={"setupCompleted": False}
            ), patch.object(bootstrap, "register", return_value="org"), patch.object(
                bootstrap, "login", return_value="token"
            ), patch.object(bootstrap, "service_account", return_value="account"), patch.object(
                bootstrap, "grant_role"
            ), patch.object(bootstrap, "create_key", return_value="minted-key"), patch.dict(
                os.environ, {"SIGNOZ_API_KEY": ""}
            ), redirect_stdout(io.StringIO()):
                self.assertEqual(bootstrap.main(), 0)
            self.assertEqual((key_file.stat().st_uid, key_file.stat().st_gid),
                             (previous.st_uid, previous.st_gid))
            self.assertEqual(stat.S_IMODE(key_file.stat().st_mode), 0o600)
            self.assertEqual(key_file.read_text(), "minted-key")

    def test_replace_failure_removes_temporary_key(self):
        with tempfile.TemporaryDirectory() as directory:
            key_file = Path(directory) / "key"
            key_file.touch()
            output = io.StringIO()
            with patch.object(bootstrap, "KEY_FILE", str(key_file)), patch.object(
                bootstrap, "wait_ready", return_value={"setupCompleted": False}
            ), patch.object(bootstrap, "register", return_value="org"), patch.object(
                bootstrap, "login", return_value="token"
            ), patch.object(bootstrap, "service_account", return_value="account"), patch.object(
                bootstrap, "grant_role"
            ), patch.object(bootstrap, "create_key", return_value="minted-key"), patch.object(
                bootstrap.os, "replace", side_effect=OSError("replace failed")
            ), patch.dict(os.environ, {"SIGNOZ_API_KEY": ""}), redirect_stdout(output):
                with self.assertRaisesRegex(OSError, "replace failed"):
                    bootstrap.main()
            self.assertEqual(key_file.read_text(), "")
            self.assertEqual(list(Path(directory).iterdir()), [key_file])
            self.assertNotIn("minted-key", output.getvalue())


if __name__ == "__main__":
    unittest.main()