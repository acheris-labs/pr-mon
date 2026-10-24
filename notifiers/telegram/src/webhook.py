"""Receive Telegram webhook calls and route button taps to per-app SQS queues.

This function sits behind a public Function URL, so anyone can reach it. Two
checks decide what gets through:

1. The secret Telegram sends in X-Telegram-Bot-Api-Secret-Token on every call
   (registered with setWebhook). No match: 401.
2. The sender's Telegram user id. Not on the allow list: dropped with a 200,
   so Telegram does not keep retrying it.

A tap is routed by its callback data: everything before the first ":" names
the route, so a button sent with data "pr-mon:k7" lands on the pr-mon queue.
Each app that sends buttons prefixes their data with its route and long-polls
its own queue. The update is queued untouched.

This function never holds the bot token. It answers a tap by returning the
answerCallbackQuery call in its HTTP response, which Telegram performs on its
behalf, so the spinner on the button stops even while the consumer is offline.
"""
import base64
import binascii
import hmac
import json
import os

import boto3

SECRET_PARAM = os.environ.get("SECRET_PARAM", "/telegram-webhook/secret-token")
SECRET_HEADER = "x-telegram-bot-api-secret-token"
ROUTE_SEPARATOR = ":"

# Terraform seeds the secret with a value starting with this. Until it is
# replaced the secret is public knowledge, so every call is refused.
PLACEHOLDER_PREFIX = "placeholder"

# Shown as a toast in Telegram when a button is tapped.
ACK_TEXT = "Sent to {route}"
NO_ROUTE_TEXT = "Nothing handles this button"


def parse_user_ids(text):
    return frozenset(int(part) for part in text.split(",") if part.strip())


def parse_routes(text):
    """Route name -> queue URL, from the JSON object Terraform writes."""
    return json.loads(text) if text.strip() else {}


ALLOWED_USER_IDS = parse_user_ids(os.environ.get("ALLOWED_USER_IDS", ""))
ROUTES = parse_routes(os.environ.get("ROUTES", ""))

ssm = boto3.client("ssm")
sqs = boto3.client("sqs")

# The secret is cached for the life of the execution environment; a rotated
# secret takes effect on the next cold start.
_parameter_cache = {}


def get_parameter(name):
    if name in _parameter_cache:
        return _parameter_cache[name]
    value = ssm.get_parameter(Name=name, WithDecryption=True)["Parameter"]["Value"]
    # Not cached while it's the placeholder, so seeding takes effect at once
    # instead of after the warm environments expire.
    if not value.startswith(PLACEHOLDER_PREFIX):
        _parameter_cache[name] = value
    return value


def respond(status, body=None):
    if body is None:
        return {"statusCode": status}
    return {"statusCode": status, "headers": {"Content-Type": "application/json"}, "body": json.dumps(body)}


def answer(callback, text):
    return respond(200, {"method": "answerCallbackQuery", "callback_query_id": callback["id"], "text": text})


def sender_id(update):
    """The user id of whoever caused the update, or None.

    An update holds update_id plus one object (message, callback_query, ...);
    every kind sent by a person carries a "from" user.
    """
    for key, value in update.items():
        if key == "update_id" or not isinstance(value, dict):
            continue
        sender = value.get("from")
        if isinstance(sender, dict) and isinstance(sender.get("id"), int):
            return sender["id"]
    return None


def route_of(callback):
    data = callback.get("data")
    if not isinstance(data, str) or ROUTE_SEPARATOR not in data:
        return None
    return data.split(ROUTE_SEPARATOR, 1)[0]


def read_body(event):
    body = event.get("body") or ""
    if event.get("isBase64Encoded"):
        body = base64.b64decode(body, validate=True).decode("utf-8")
    return json.loads(body)


def lambda_handler(event, context):
    method = event.get("requestContext", {}).get("http", {}).get("method")
    if method != "POST":
        return respond(405)

    secret = get_parameter(SECRET_PARAM)
    if secret.startswith(PLACEHOLDER_PREFIX):
        print(json.dumps({"refused": "secret not seeded"}))
        return respond(503)

    headers = {name.lower(): value for name, value in (event.get("headers") or {}).items()}
    supplied = headers.get(SECRET_HEADER, "")
    if not hmac.compare_digest(supplied.encode("utf-8"), secret.encode("utf-8")):
        return respond(401)

    try:
        update = read_body(event)
    except (ValueError, binascii.Error):
        return respond(400)
    if not isinstance(update, dict) or not isinstance(update.get("update_id"), int):
        return respond(400)

    user = sender_id(update)
    if user not in ALLOWED_USER_IDS:
        print(json.dumps({"dropped": "sender not allowed", "user_id": user, "update_id": update["update_id"]}))
        return respond(200)

    callback = update.get("callback_query")
    if not isinstance(callback, dict) or "id" not in callback:
        print(json.dumps({"dropped": "not a button tap", "update_id": update["update_id"]}))
        return respond(200)

    route = route_of(callback)
    if route not in ROUTES:
        print(json.dumps({"dropped": "no such route", "route": route, "update_id": update["update_id"]}))
        return answer(callback, NO_ROUTE_TEXT)

    # A failure here raises, Lambda returns a 5xx, and Telegram retries; the
    # dedup id keeps the retry from being queued twice.
    sqs.send_message(
        QueueUrl=ROUTES[route],
        MessageBody=json.dumps(update),
        MessageGroupId=str(user),
        MessageDeduplicationId=str(update["update_id"]),
    )
    return answer(callback, ACK_TEXT.format(route=route))
