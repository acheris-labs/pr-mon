"""Point the bot's webhook at the receiver's Function URL, or inspect or remove it.

Run by hand with `aws lambda invoke`; it has no Function URL, so only an IAM
identity allowed to invoke it can. It runs in AWS, so the bot token (kept in
SSM) never leaves AWS and the operator's machine never talks to
api.telegram.org.

Event: {"action": "set" | "info" | "delete"}, default "info". Every action
returns the webhook's state afterwards, from getWebhookInfo.
"""
import json
import os
import urllib.error
import urllib.request

import boto3

BOT_TOKEN_PARAM = os.environ.get("BOT_TOKEN_PARAM", "/telegram-webhook/bot-token")
SECRET_PARAM = os.environ.get("SECRET_PARAM", "/telegram-webhook/secret-token")
WEBHOOK_URL = os.environ.get("WEBHOOK_URL", "")
TELEGRAM_API = os.environ.get("TELEGRAM_API", "https://api.telegram.org")

PLACEHOLDER_PREFIX = "placeholder"

# Only button taps are delivered. Widen this (e.g. add "message") to let
# pr-mon receive typed commands too.
ALLOWED_UPDATES = ["callback_query"]

TIMEOUT_SECONDS = 10

ssm = boto3.client("ssm")

_parameter_cache = {}


class RegisterError(Exception):
    """The Bot API call failed."""


def get_parameter(name):
    if name in _parameter_cache:
        return _parameter_cache[name]
    value = ssm.get_parameter(Name=name, WithDecryption=True)["Parameter"]["Value"]
    # Not cached while it's the placeholder, so seeding takes effect at once.
    if not value.startswith(PLACEHOLDER_PREFIX):
        _parameter_cache[name] = value
    return value


def call(method, params):
    token = get_parameter(BOT_TOKEN_PARAM)
    request = urllib.request.Request(
        f"{TELEGRAM_API}/bot{token}/{method}",
        data=json.dumps(params).encode(),
        headers={"Content-Type": "application/json"},
    )
    # Errors name the method, never the URL: the URL holds the token.
    try:
        with urllib.request.urlopen(request, timeout=TIMEOUT_SECONDS) as response:
            body = json.loads(response.read())
    except urllib.error.HTTPError as error:
        raise RegisterError(
            f"{method}: telegram returned {error.code}: {error.read().decode('utf-8', 'replace')}"
        ) from None
    except urllib.error.URLError as error:
        raise RegisterError(f"{method}: telegram unreachable: {error.reason}") from None

    if not body.get("ok"):
        raise RegisterError(f"{method}: telegram rejected the call: {body}")
    return body["result"]


def lambda_handler(event, context):
    action = (event or {}).get("action", "info")

    if action == "set":
        secret = get_parameter(SECRET_PARAM)
        if secret.startswith(PLACEHOLDER_PREFIX):
            raise RegisterError(f"seed {SECRET_PARAM} before registering (see README)")
        call("setWebhook", {
            "url": WEBHOOK_URL,
            "secret_token": secret,
            "allowed_updates": ALLOWED_UPDATES,
            "drop_pending_updates": True,
        })
    elif action == "delete":
        call("deleteWebhook", {})
    elif action != "info":
        raise RegisterError(f"unknown action {action!r}; use set, info or delete")

    return {"ok": True, "action": action, "webhook": call("getWebhookInfo", {})}
