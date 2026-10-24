"""Tests for the public webhook receiver. No network, no AWS."""
import base64
import json
import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))
# boto3 needs a region to build its clients at import time; nothing is called.
os.environ.setdefault("AWS_DEFAULT_REGION", "us-east-1")

import webhook   # noqa: E402  (path and region must be set before import)

SECRET = "s3cret-token_value"
ME = 1111
STRANGER = 2222
ROUTES = {
    "pr-mon": "https://sqs.example/telegram-webhook-pr-mon.fifo",
    "backup": "https://sqs.example/telegram-webhook-backup.fifo",
}


def url_event(body, secret=SECRET, method="POST", base64_encoded=False, header="X-Telegram-Bot-Api-Secret-Token"):
    """A Function URL (payload 2.0) event as Lambda delivers it."""
    text = json.dumps(body) if not isinstance(body, str) else body
    headers = {"content-type": "application/json"}
    if secret is not None:
        headers[header] = secret
    return {
        "version": "2.0",
        "headers": headers,
        "requestContext": {"http": {"method": method}},
        "body": base64.b64encode(text.encode()).decode() if base64_encoded else text,
        "isBase64Encoded": base64_encoded,
    }


def callback_update(data="pr-mon:k7", user_id=ME, update_id=500):
    query = {"id": "cbq-1", "from": {"id": user_id, "is_bot": False, "first_name": "C"}}
    if data is not None:
        query["data"] = data
    return {"update_id": update_id, "callback_query": query}


class WebhookTestCase(unittest.TestCase):
    def setUp(self):
        webhook._parameter_cache.clear()
        webhook._parameter_cache[webhook.SECRET_PARAM] = SECRET
        sqs_patch = mock.patch.object(webhook, "sqs")
        self.sqs = sqs_patch.start()
        self.addCleanup(sqs_patch.stop)
        for name, value in [("ROUTES", ROUTES), ("ALLOWED_USER_IDS", frozenset({ME}))]:
            patch = mock.patch.object(webhook, name, value)
            patch.start()
            self.addCleanup(patch.stop)

    def answer(self, result):
        self.assertEqual(result["statusCode"], 200)
        body = json.loads(result["body"])
        self.assertEqual(body["method"], "answerCallbackQuery")
        self.assertEqual(body["callback_query_id"], "cbq-1")
        return body["text"]

    def test_queues_tap_on_its_route_and_answers_it(self):
        result = webhook.lambda_handler(url_event(callback_update()), None)

        self.assertEqual(self.answer(result), "Sent to pr-mon")
        self.sqs.send_message.assert_called_once()
        sent = self.sqs.send_message.call_args.kwargs
        self.assertEqual(sent["QueueUrl"], ROUTES["pr-mon"])
        self.assertEqual(json.loads(sent["MessageBody"]), callback_update())
        self.assertEqual(sent["MessageGroupId"], str(ME))
        # Telegram redelivers an update it thinks failed; the FIFO queue drops
        # the repeat because the dedup id is the update id.
        self.assertEqual(sent["MessageDeduplicationId"], "500")

    def test_each_route_goes_to_its_own_queue(self):
        webhook.lambda_handler(url_event(callback_update(data="backup:run")), None)
        self.assertEqual(self.sqs.send_message.call_args.kwargs["QueueUrl"], ROUTES["backup"])

    def test_route_is_everything_before_the_first_colon(self):
        webhook.lambda_handler(url_event(callback_update(data="pr-mon:a:b:c")), None)
        self.assertEqual(self.sqs.send_message.call_args.kwargs["QueueUrl"], ROUTES["pr-mon"])

    def test_tap_with_no_known_route_is_answered_but_not_queued(self):
        for data in ["nobody:x", "pr-mon", "", None, ":x"]:
            with self.subTest(data=data):
                result = webhook.lambda_handler(url_event(callback_update(data=data)), None)
                self.assertEqual(self.answer(result), webhook.NO_ROUTE_TEXT)
        self.sqs.send_message.assert_not_called()

    def test_updates_other_than_taps_are_dropped(self):
        update = {"update_id": 7, "message": {"message_id": 1, "from": {"id": ME}, "text": "pr-mon:hi"}}
        self.assertEqual(webhook.lambda_handler(url_event(update), None), {"statusCode": 200})
        self.sqs.send_message.assert_not_called()

    def test_header_name_case_does_not_matter(self):
        event = url_event(callback_update(), header="x-telegram-bot-api-secret-token")
        self.assertEqual(webhook.lambda_handler(event, None)["statusCode"], 200)
        self.sqs.send_message.assert_called_once()

    def test_base64_body_is_decoded(self):
        result = webhook.lambda_handler(url_event(callback_update(), base64_encoded=True), None)
        self.assertEqual(result["statusCode"], 200)
        self.sqs.send_message.assert_called_once()

    def test_rejects_missing_or_wrong_secret(self):
        for secret in [None, "", "wrong", SECRET + "x", "é"]:
            with self.subTest(secret=secret):
                result = webhook.lambda_handler(url_event(callback_update(), secret=secret), None)
                self.assertEqual(result["statusCode"], 401)
        self.sqs.send_message.assert_not_called()

    def test_refuses_everything_while_secret_is_the_placeholder(self):
        placeholder = webhook.PLACEHOLDER_PREFIX + "-seed-me"
        webhook._parameter_cache[webhook.SECRET_PARAM] = placeholder

        self.assertEqual(webhook.lambda_handler(url_event(callback_update(), secret=placeholder), None)["statusCode"], 503)
        self.sqs.send_message.assert_not_called()

    def test_rejects_non_post(self):
        result = webhook.lambda_handler(url_event(callback_update(), method="GET"), None)
        self.assertEqual(result["statusCode"], 405)
        self.sqs.send_message.assert_not_called()

    def test_rejects_malformed_bodies(self):
        for body in ["not json", "[]", "{}", json.dumps({"update_id": "7"}), ""]:
            with self.subTest(body=body):
                self.assertEqual(webhook.lambda_handler(url_event(body), None)["statusCode"], 400)
        self.sqs.send_message.assert_not_called()

    def test_rejects_bad_base64(self):
        event = url_event("x")
        event["isBase64Encoded"] = True
        event["body"] = "!!!not base64!!!"
        self.assertEqual(webhook.lambda_handler(event, None)["statusCode"], 400)

    def test_drops_strangers_with_200_so_telegram_does_not_retry(self):
        with mock.patch("builtins.print") as printed:
            result = webhook.lambda_handler(url_event(callback_update(user_id=STRANGER)), None)

        # No answer either: a stranger learns nothing, not even that the bot heard.
        self.assertEqual(result, {"statusCode": 200})
        self.sqs.send_message.assert_not_called()
        # The id is logged: it's how you find your own id when setting up.
        self.assertIn(str(STRANGER), printed.call_args[0][0])

    def test_drops_updates_with_no_sender(self):
        update = {"update_id": 9, "channel_post": {"message_id": 1, "text": "hi"}}
        self.assertEqual(webhook.lambda_handler(url_event(update), None), {"statusCode": 200})
        self.sqs.send_message.assert_not_called()

    def test_drops_everything_when_no_users_are_allowed(self):
        with mock.patch.object(webhook, "ALLOWED_USER_IDS", frozenset()):
            result = webhook.lambda_handler(url_event(callback_update()), None)
        self.assertEqual(result, {"statusCode": 200})
        self.sqs.send_message.assert_not_called()

    def test_queue_failure_raises_so_telegram_retries(self):
        self.sqs.send_message.side_effect = RuntimeError("sqs down")
        with self.assertRaises(RuntimeError):
            webhook.lambda_handler(url_event(callback_update()), None)

    def test_secret_is_fetched_once_and_cached(self):
        webhook._parameter_cache.clear()
        with mock.patch.object(webhook, "ssm") as ssm:
            ssm.get_parameter.return_value = {"Parameter": {"Value": SECRET}}
            webhook.lambda_handler(url_event(callback_update(update_id=1)), None)
            webhook.lambda_handler(url_event(callback_update(update_id=2)), None)

        ssm.get_parameter.assert_called_once_with(Name=webhook.SECRET_PARAM, WithDecryption=True)

    def test_seeding_the_secret_takes_effect_without_a_cold_start(self):
        webhook._parameter_cache.clear()
        placeholder = webhook.PLACEHOLDER_PREFIX + "-seed-me"
        with mock.patch.object(webhook, "ssm") as ssm:
            ssm.get_parameter.side_effect = [
                {"Parameter": {"Value": placeholder}},
                {"Parameter": {"Value": SECRET}},
            ]
            before = webhook.lambda_handler(url_event(callback_update(update_id=1)), None)
            after = webhook.lambda_handler(url_event(callback_update(update_id=2)), None)

        self.assertEqual(before["statusCode"], 503)
        self.assertEqual(after["statusCode"], 200)
        self.sqs.send_message.assert_called_once()


class ConfigParsingTestCase(unittest.TestCase):
    def test_parses_comma_separated_user_ids(self):
        self.assertEqual(webhook.parse_user_ids("1, 22,333"), frozenset({1, 22, 333}))

    def test_empty_user_ids_means_nobody(self):
        for text in ["", " ", ",,"]:
            with self.subTest(text=text):
                self.assertEqual(webhook.parse_user_ids(text), frozenset())

    def test_parses_routes(self):
        self.assertEqual(webhook.parse_routes(json.dumps(ROUTES)), ROUTES)

    def test_empty_routes_means_none(self):
        self.assertEqual(webhook.parse_routes(""), {})


if __name__ == "__main__":
    unittest.main()
