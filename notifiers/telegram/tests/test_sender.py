"""Tests for the sender Lambda. No network, no AWS."""
import io
import json
import sys
import unittest
import urllib.error
import urllib.parse
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))

import sender   # noqa: E402  (path must be set before import)


class HandlerTestCase(unittest.TestCase):
    def setUp(self):
        sender._parameter_cache.clear()
        sender._parameter_cache[sender.BOT_TOKEN_PARAM] = "test-token"
        patcher = mock.patch.object(sender, "DEFAULT_CHAT_ID", "default-chat")
        patcher.start()
        self.addCleanup(patcher.stop)

    def fake_response(self, payload):
        response = mock.MagicMock()
        response.read.return_value = json.dumps(payload).encode()
        response.__enter__.return_value = response
        return response

    def test_sends_message_and_returns_message_id(self):
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = self.fake_response({"ok": True, "result": {"message_id": 42}})
            result = sender.lambda_handler({"message": "hello"}, None)

        self.assertEqual(result, {"ok": True, "message_id": 42, "destination": "default-chat"})
        request = urlopen.call_args[0][0]
        self.assertIn("/bottest-token/sendMessage", request.full_url)
        self.assertIn(b"chat_id=default-chat", request.data)
        self.assertIn(b"text=hello", request.data)

    def test_explicit_destination_overrides_default(self):
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = self.fake_response({"ok": True, "result": {"message_id": 1}})
            result = sender.lambda_handler({"message": "hi", "destination": "9999"}, None)

        self.assertEqual(result["destination"], "9999")
        self.assertIn(b"chat_id=9999", urlopen.call_args[0][0].data)

    def test_passes_buttons_through_as_reply_markup(self):
        markup = {"inline_keyboard": [[{"text": "Yes", "callback_data": "test:yes"}]]}
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = self.fake_response({"ok": True, "result": {"message_id": 1}})
            sender.lambda_handler({"message": "pick", "reply_markup": markup}, None)

        sent = urllib.parse.parse_qs(urlopen.call_args[0][0].data.decode())
        self.assertEqual(json.loads(sent["reply_markup"][0]), markup)

    def test_no_reply_markup_unless_given(self):
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = self.fake_response({"ok": True, "result": {"message_id": 1}})
            sender.lambda_handler({"message": "plain"}, None)

        self.assertNotIn("reply_markup", urllib.parse.parse_qs(urlopen.call_args[0][0].data.decode()))

    def test_rejects_reply_markup_that_is_not_an_object(self):
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            for markup in ["[]", [], 7]:
                with self.subTest(markup=markup), self.assertRaises(sender.SendError):
                    sender.lambda_handler({"message": "hi", "reply_markup": markup}, None)

        urlopen.assert_not_called()

    def test_passes_parse_mode_through(self):
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = self.fake_response({"ok": True, "result": {"message_id": 1}})
            sender.lambda_handler({"message": "<b>hi</b>", "parse_mode": "HTML"}, None)

        sent = urllib.parse.parse_qs(urlopen.call_args[0][0].data.decode())
        self.assertEqual(sent["parse_mode"], ["HTML"])

    def test_message_id_edits_instead_of_sending(self):
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = self.fake_response({"ok": True, "result": {"message_id": 1068}})
            result = sender.lambda_handler({"message": "done", "message_id": 1068}, None)

        request = urlopen.call_args[0][0]
        self.assertTrue(request.full_url.endswith("/editMessageText"))
        sent = urllib.parse.parse_qs(request.data.decode())
        self.assertEqual(sent["message_id"], ["1068"])
        self.assertEqual(sent["chat_id"], ["default-chat"])
        # No reply_markup on an edit means Telegram removes the buttons.
        self.assertNotIn("reply_markup", sent)
        self.assertEqual(result["message_id"], 1068)

    def test_sends_new_message_without_message_id(self):
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = self.fake_response({"ok": True, "result": {"message_id": 1}})
            sender.lambda_handler({"message": "new"}, None)

        self.assertTrue(urlopen.call_args[0][0].full_url.endswith("/sendMessage"))

    def test_rejects_bad_message_id_or_parse_mode(self):
        events = [
            {"message": "x", "message_id": "1068"},
            {"message": "x", "message_id": True},
            {"message": "x", "parse_mode": 1},
        ]
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            for event in events:
                with self.subTest(event=event), self.assertRaises(sender.SendError):
                    sender.lambda_handler(event, None)

        urlopen.assert_not_called()

    def test_rejects_missing_or_blank_message(self):
        for event in [{}, {"message": ""}, {"message": "   "}, {"message": 7}, None]:
            with self.subTest(event=event), self.assertRaises(sender.SendError):
                sender.lambda_handler(event, None)

    def test_truncates_overlong_message(self):
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = self.fake_response({"ok": True, "result": {"message_id": 1}})
            sender.lambda_handler({"message": "x" * 5000}, None)

        sent = urllib.parse.parse_qs(urlopen.call_args[0][0].data.decode())["text"][0]
        self.assertEqual(len(sent), sender.MAX_MESSAGE_CHARS)
        self.assertTrue(sent.endswith("..."))

    def test_raises_on_telegram_rejection(self):
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = self.fake_response({"ok": False, "description": "chat not found"})
            with self.assertRaises(sender.SendError) as caught:
                sender.lambda_handler({"message": "hi"}, None)

        self.assertIn("chat not found", str(caught.exception))

    def test_raises_with_body_on_http_error(self):
        error = urllib.error.HTTPError(
            "url", 401, "Unauthorized", {}, io.BytesIO(b'{"description":"bot token invalid"}')
        )
        with mock.patch.object(sender.urllib.request, "urlopen", side_effect=error):
            with self.assertRaises(sender.SendError) as caught:
                sender.lambda_handler({"message": "hi"}, None)

        self.assertIn("401", str(caught.exception))
        self.assertIn("bot token invalid", str(caught.exception))

    def test_raises_when_telegram_unreachable(self):
        with mock.patch.object(sender.urllib.request, "urlopen", side_effect=urllib.error.URLError("timed out")):
            with self.assertRaises(sender.SendError) as caught:
                sender.lambda_handler({"message": "hi"}, None)

        self.assertIn("unreachable", str(caught.exception))

    def test_delete_calls_delete_message(self):
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = self.fake_response({"ok": True, "result": True})
            result = sender.lambda_handler({"delete": True, "message_id": 1068}, None)

        request = urlopen.call_args[0][0]
        self.assertTrue(request.full_url.endswith("/deleteMessage"))
        sent = urllib.parse.parse_qs(request.data.decode())
        self.assertEqual(sent["chat_id"], ["default-chat"])
        self.assertEqual(sent["message_id"], ["1068"])
        self.assertNotIn("text", sent)
        self.assertEqual(result, {"ok": True, "message_id": 1068, "destination": "default-chat"})

    def test_delete_needs_message_id(self):
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            for event in [{"delete": True}, {"delete": True, "message_id": "5"}, {"delete": True, "message_id": True}]:
                with self.subTest(event=event), self.assertRaises(sender.SendError):
                    sender.lambda_handler(event, None)

        urlopen.assert_not_called()

    def test_delete_ignores_message(self):
        with mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            urlopen.return_value = self.fake_response({"ok": True, "result": True})
            sender.lambda_handler({"delete": True, "message_id": 5, "message": "x"}, None)

        sent = urllib.parse.parse_qs(urlopen.call_args[0][0].data.decode())
        self.assertNotIn("text", sent)

    def test_delete_refusal_surfaces_telegram_text(self):
        error = urllib.error.HTTPError(
            "url", 400, "Bad Request", {},
            io.BytesIO(b'{"description":"Bad Request: message can\'t be deleted for everyone"}'),
        )
        with mock.patch.object(sender.urllib.request, "urlopen", side_effect=error):
            with self.assertRaises(sender.SendError) as caught:
                sender.lambda_handler({"delete": True, "message_id": 5}, None)

        self.assertIn("message can't be deleted", str(caught.exception))

    def test_token_is_fetched_once_and_cached(self):
        sender._parameter_cache.clear()
        with mock.patch.object(sender, "ssm") as ssm, mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            ssm.get_parameter.return_value = {"Parameter": {"Value": "v"}}
            urlopen.return_value = self.fake_response({"ok": True, "result": {"message_id": 1}})
            sender.lambda_handler({"message": "a"}, None)
            sender.lambda_handler({"message": "b"}, None)

        # Only the token is a parameter; fetched once across two invocations.
        self.assertEqual(ssm.get_parameter.call_count, 1)

    def test_no_destination_and_no_default_chat_fails(self):
        with mock.patch.object(sender, "DEFAULT_CHAT_ID", ""), \
                mock.patch.object(sender.urllib.request, "urlopen") as urlopen:
            with self.assertRaises(sender.SendError):
                sender.lambda_handler({"message": "hello"}, None)
            with self.assertRaises(sender.SendError):
                sender.lambda_handler({"delete": True, "message_id": 5}, None)
        urlopen.assert_not_called()


if __name__ == "__main__":
    unittest.main()
