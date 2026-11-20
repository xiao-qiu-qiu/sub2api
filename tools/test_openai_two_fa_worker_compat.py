import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import call, patch

from tools import openai_codex_oauth_adapter as adapter
from tools import openai_oauth_reauth_worker as worker


class InitialTwoFAWorkerCompatibilityTest(unittest.TestCase):
    def test_initial_login_uses_local_codex_worker_and_submits_credentials(self):
        self.check_initial_login(worker)

    def check_initial_login(self, runtime):
        task_id = (1 << 53) - 1
        claim = {
            "task_id": task_id,
            "account_id": task_id,
            "credential_mode": "password_totp",
            "engine": "local_worker",
            "oauth_profile": "codex",
            "login_email": "fixture@example.invalid",
            "password": "fixture-password",
            "totp_secret": "fixture-totp",
            "proxy_url": "http://proxy.example.invalid:3128",
        }
        credentials = {
            "access_token": "fixture-access",
            "refresh_token": "fixture-refresh",
            "id_token": "fixture-id",
        }
        extra = {"client_id": "fixture-client"}
        ambient = {
            "NODE_EXECUTABLE": "fixture-node",
            "OPENAI_REAUTH_WORKER_TOKEN": "fixture-worker-token",
            "CHATGPT_PROXY_URL": "http://ambient.example.invalid:3128",
            "OPENAI_CODEX_EXPECTED_WORKSPACE": "ambient-workspace",
        }

        with tempfile.TemporaryDirectory() as root_dir:
            root = Path(root_dir).resolve()
            script = root / "src" / "protocol-login.mjs"
            script.parent.mkdir()
            script.touch()
            api = runtime.WorkerAPI(SimpleNamespace(tosub2_root=root, worker_id="fixture-worker"))

            def fake_runner(command, *, cwd, env, **_kwargs):
                output_path = Path(cwd) / "oauth.json"
                self.assertEqual(command, [
                    "fixture-node", str(script), "--email", claim["login_email"],
                    "--output-mode", "sub2api", "--sub2api-out", str(output_path),
                    "--sub2api-name", f"credential-guard-{task_id}", "--proxy", claim["proxy_url"],
                ])
                self.assertEqual(env["CHATGPT_LOGIN_PASSWORD"], claim["password"])
                self.assertEqual(env["CHATGPT_TOTP_SECRET"], claim["totp_secret"])
                self.assertEqual(env["CHATGPT_PROXY_MAX_ATTEMPTS"], "1")
                self.assertEqual(env["SUB2API_PROTOCOL_TASK_ID"], str(task_id))
                for key in ambient.keys() - {"NODE_EXECUTABLE"}:
                    self.assertNotIn(key, env)
                diagnostic = Path(env["SUB2API_PROTOCOL_DIAGNOSTIC_PATH"])
                self.assertEqual(diagnostic.parent, Path(cwd))
                diagnostic.write_text(json.dumps({
                    "version": 1, "task_id": task_id, "stage": "completed", "reason": "none", "elapsed_ms": 1,
                }), encoding="utf-8")
                output_path.write_text(json.dumps({
                    "accounts": [{"credentials": credentials, "extra": extra}],
                }), encoding="utf-8")
                return SimpleNamespace(returncode=0)

            with (
                patch.dict(runtime.os.environ, ambient, clear=True),
                patch.object(runtime.shutil, "which", return_value="fixture-node"),
                patch.object(adapter, "prepare_codex_runtime", return_value=script),
                patch.object(runtime.subprocess, "run", side_effect=fake_runner) as runner,
                patch.object(runtime, "process_session_studio_claim") as session_studio,
                patch.object(api, "_post", return_value={"status": "succeeded"}) as post,
            ):
                runtime.process_claim(api, None, claim)

        runner.assert_called_once()
        session_studio.assert_not_called()
        task_path = f"/api/v1/internal/openai-reauth/{task_id}"
        self.assertEqual(post.call_args_list, [
            *(call(f"{task_path}/progress", {"worker_id": "fixture-worker", "stage": stage})
              for stage in ("starting", "protocol_connecting", "applying_credentials")),
            call(f"{task_path}/credentials", {"worker_id": "fixture-worker", "credentials": credentials, "extra": extra}),
        ])


if __name__ == "__main__":
    unittest.main()
