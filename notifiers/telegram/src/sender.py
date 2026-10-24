"""Send, edit and delete Telegram messages for the pr-mon handler.

The bot token lives in SSM Parameter Store and is read here at runtime, so it
never sits on the calling machine. The handler signs a direct
lambda:InvokeFunction request with its route's reader key.
"""
import json
import os
import urllib.error
import urllib.parse
import urllib.request

import boto3

BOT_TOKEN_PARAM = os.environ.get("BOT_TOKEN_PARAM", "/telegram-webhook/bot-token")
# Where messages go when the caller names no destination. Not a secret.
DEFAULT_CHAT_ID = os.environ.get("DEFAULT_CHAT_ID", "")
TELEGRAM_API = os.environ.get("TELEGRAM_API", "https://api.telegram.org")

TIMEOUT_SECONDS = 10
MAX_MESSAGE_CHARS = 4096   # Telegram rejects sendMessage text longer than this.

ssm = boto3.client("ssm")

# The token is cached for the life of the execution environment: a rotated
# token takes effect on the next cold start, which is the right trade for a
# notification path that would otherwise hit SSM on every send.
_parameter_cache = {}


class SendError(Exception):
    """Message could not be delivered."""


def get_parameter(name):
    if name not in _parameter_cache:
        response = ssm.get_parameter(Name=name, WithDecryption=True)
        _parameter_cache[name] = response["Parameter"]["Value"]
    return _parameter_cache[name]


def call(method, fields):
    """POST a Bot API method and return its result."""
    token = get_parameter(BOT_TOKEN_PARAM)
    payload = urllib.parse.urlencode(fields).encode()
    request = urllib.request.Request(
        f"{TELEGRAM_API}/bot{token}/{method}",
        data=payload,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )

    try:
        with urllib.request.urlopen(request, timeout=TIMEOUT_SECONDS) as response:
            body = json.loads(response.read())
    except urllib.error.HTTPError as error:
        # Telegram puts the useful reason in the body, not the status line.
        raise SendError(f"telegram returned {error.code}: {error.read().decode('utf-8', 'replace')}") from error
    except urllib.error.URLError as error:
        raise SendError(f"telegram unreachable: {error.reason}") from error

    if not body.get("ok"):
        raise SendError(f"telegram rejected the request: {body}")
    return body["result"]


def send(message, destination, reply_markup=None, parse_mode=None, message_id=None):
    """Send a message, or with message_id replace that message's text and buttons."""
    fields = {"chat_id": destination, "text": message}
    if reply_markup is not None:
        # Telegram takes the markup as a JSON string inside the form body.
        fields["reply_markup"] = json.dumps(reply_markup)
    if parse_mode is not None:
        fields["parse_mode"] = parse_mode
    if message_id is not None:
        fields["message_id"] = message_id
    method = "sendMessage" if message_id is None else "editMessageText"
    return call(method, fields)["message_id"]


def delete(message_id, destination):
    """Delete a message. Telegram's refusal text stays in the raised SendError."""
    call("deleteMessage", {"chat_id": destination, "message_id": message_id})


def destination_of(event):
    destination = event.get("destination") or DEFAULT_CHAT_ID
    if not destination:
        raise SendError("no destination and no default chat configured")
    return destination


def lambda_handler(event, context):
    event = event or {}

    if event.get("delete") is True:
        message_id = event.get("message_id")
        if not isinstance(message_id, int) or isinstance(message_id, bool):
            raise SendError("message_id is required and must be an integer to delete")
        destination = destination_of(event)
        delete(message_id, destination)
        return {"ok": True, "message_id": message_id, "destination": destination}

    message = event.get("message")
    if not isinstance(message, str) or not message.strip():
        raise SendError("message is required and must be a non-empty string")
    if len(message) > MAX_MESSAGE_CHARS:
        message = message[: MAX_MESSAGE_CHARS - 3] + "..."

    # Optional buttons, passed to Telegram as given, e.g.
    # {"inline_keyboard": [[{"text": "Merge", "callback_data": "pr-mon:k7"}]]}
    reply_markup = event.get("reply_markup")
    if reply_markup is not None and not isinstance(reply_markup, dict):
        raise SendError("reply_markup must be an object")

    # Optional formatting: "HTML" or "MarkdownV2", passed to Telegram as given.
    parse_mode = event.get("parse_mode")
    if parse_mode is not None and not isinstance(parse_mode, str):
        raise SendError("parse_mode must be a string")

    # Optional: edit this earlier message instead of sending a new one. Without
    # reply_markup the edit removes the message's buttons.
    message_id = event.get("message_id")
    if message_id is not None and (not isinstance(message_id, int) or isinstance(message_id, bool)):
        raise SendError("message_id must be an integer")

    destination = destination_of(event)

    sent_id = send(message, destination, reply_markup, parse_mode, message_id)
    return {"ok": True, "message_id": sent_id, "destination": destination}
