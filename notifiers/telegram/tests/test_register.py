"""Tests for the webhook registration function. No network, no AWS."""
import io
import json
import os
import sys
import unittest
import urllib.error
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))
os.environ.setdefault("AWS_DEFAULT_REGION", "us-east-1")

import register   # noqa: E402  (path and region must be set before import)

WEBHOOK_INFO = {"url": "https://abc.lambda-url.us-east-1.on.aws/", "pending_update_count": 0}


def fake_response(payload):
    response = mock.MagicMock()
    response.read.return_value = json.dumps(payload).encode()
    response.__enter__.return_value = response
    return response


class RegisterTestCase(unittest.TestCase):
    def setUp(self):
        register._parameter_cache.clear()
        register._parameter_cache[register.BOT_TOKEN_PARAM] = "test-token"
        register._parameter_cache[register.SECRET_PARAM] = "the-secret"
        patch = mock.patch.object(register, "WEBHOOK_URL", WEBHOOK_INFO["url"])
        patch.start()
        self.addCleanup(patch.stop)

    def calls(self, urlopen):
        """(method, params) for each Bot API request made."""
        made = []
        for call in urlopen.call_args_list:
            request = call[0][0]
            made.append((request.full_url.rsplit("/", 1)[1], json.loads(request.data)))
        return made

    def test_set_registers_url_secret_and_update_types(self):
        with mock.patch.object(register.urllib.request, "urlopen") as urlopen:
            urlopen.side_effect = [
                fake_response({"ok": True, "result": True}),
                fake_response({"ok": True, "result": WEBHOOK_INFO}),
            ]
            result = register.lambda_handler({"action": "set"}, None)

        self.assertEqual(result, {"ok": True, "action": "set", "webhook": WEBHOOK_INFO})
        (method, params), (second, _) = self.calls(urlopen)
        self.assertEqual(method, "setWebhook")
        self.assertEqual(params, {
            "url": WEBHOOK_INFO["url"],
            "secret_token": "the-secret",
            "allowed_updates": register.ALLOWED_UPDATES,
            "drop_pending_updates": True,
        })
        self.assertEqual(second, "getWebhookInfo")
        self.assertIn("/bottest-token/", urlopen.call_args_list[0][0][0].full_url)

    def test_info_is_the_default(self):
        with mock.patch.object(register.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = fake_response({"ok": True, "result": WEBHOOK_INFO})
            for event in [None, {}, {"action": "info"}]:
                with self.subTest(event=event):
                    self.assertEqual(register.lambda_handler(event, None)["webhook"], WEBHOOK_INFO)

        self.assertTrue(all(method == "getWebhookInfo" for method, _ in self.calls(urlopen)))

    def test_delete_removes_the_webhook(self):
        with mock.patch.object(register.urllib.request, "urlopen") as urlopen:
            urlopen.side_effect = [
                fake_response({"ok": True, "result": True}),
                fake_response({"ok": True, "result": {"url": ""}}),
            ]
            result = register.lambda_handler({"action": "delete"}, None)

        self.assertEqual(self.calls(urlopen)[0], ("deleteWebhook", {}))
        self.assertEqual(result["webhook"], {"url": ""})

    def test_unknown_action_is_an_error(self):
        with self.assertRaises(register.RegisterError):
            register.lambda_handler({"action": "explode"}, None)

    def test_set_refuses_placeholder_secret(self):
        register._parameter_cache[register.SECRET_PARAM] = register.PLACEHOLDER_PREFIX + "-seed-me"
        with mock.patch.object(register.urllib.request, "urlopen") as urlopen:
            with self.assertRaises(register.RegisterError) as caught:
                register.lambda_handler({"action": "set"}, None)

        urlopen.assert_not_called()
        self.assertIn("seed", str(caught.exception))

    def test_seeding_the_secret_takes_effect_without_a_cold_start(self):
        del register._parameter_cache[register.SECRET_PARAM]
        with mock.patch.object(register, "ssm") as ssm, \
                mock.patch.object(register.urllib.request, "urlopen") as urlopen:
            ssm.get_parameter.side_effect = [
                {"Parameter": {"Value": register.PLACEHOLDER_PREFIX + "-seed-me"}},
                {"Parameter": {"Value": "the-secret"}},
            ]
            urlopen.side_effect = [
                fake_response({"ok": True, "result": True}),
                fake_response({"ok": True, "result": WEBHOOK_INFO}),
            ]
            with self.assertRaises(register.RegisterError):
                register.lambda_handler({"action": "set"}, None)
            result = register.lambda_handler({"action": "set"}, None)

        self.assertEqual(result["webhook"], WEBHOOK_INFO)

    def test_telegram_rejection_raises(self):
        with mock.patch.object(register.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = fake_response({"ok": False, "description": "bad webhook"})
            with self.assertRaises(register.RegisterError) as caught:
                register.lambda_handler({"action": "info"}, None)

        self.assertIn("bad webhook", str(caught.exception))

    def test_http_error_raises_without_leaking_the_token(self):
        error = urllib.error.HTTPError(
            "https://api.telegram.org/bottest-token/getWebhookInfo", 401, "Unauthorized", {},
            io.BytesIO(b'{"description":"Unauthorized"}'),
        )
        with mock.patch.object(register.urllib.request, "urlopen", side_effect=error):
            with self.assertRaises(register.RegisterError) as caught:
                register.lambda_handler({"action": "info"}, None)

        self.assertIn("401", str(caught.exception))
        self.assertNotIn("test-token", str(caught.exception))

    def test_unreachable_raises(self):
        with mock.patch.object(register.urllib.request, "urlopen", side_effect=urllib.error.URLError("timed out")):
            with self.assertRaises(register.RegisterError) as caught:
                register.lambda_handler({"action": "info"}, None)

        self.assertIn("unreachable", str(caught.exception))


if __name__ == "__main__":
    unittest.main()
