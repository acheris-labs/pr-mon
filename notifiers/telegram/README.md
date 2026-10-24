# Telegram notifier

pr-mon's two-way notifications over Telegram: a PR's notification arrives as a
Telegram message with buttons, and tapping Squash & merge (or Re-run, or
Dismiss) acts on the PR.

| Part | What it is |
|:-----|:-----------|
| `cmd/pr-mon-telegram` | The [two-way handler](../../docs/two-way-handler.md) pr-mon runs. Go, in pr-mon's module. |
| `src/webhook.py` | Lambda behind a public Function URL that receives Telegram's button taps and queues them in SQS. |
| `src/sender.py` | Lambda that sends, edits and deletes the bot's messages. |
| `src/register.py` | Lambda that sets, shows or removes the bot's webhook. |
| `*.tf` | Terraform for the Lambdas, queues, keys and roles. |

pr-mon and the handler never talk to `api.telegram.org`, never hold the bot
token, and need no public address: taps arrive on a queue the handler
long-polls, and messages go out through the sender Lambda. The bot token stays
in SSM, read only by the sender and register functions.

You need a Telegram bot: create one with @BotFather and keep its token for
[seeding](#seed-the-secrets).

Set up in this order: [the AWS side](#setup), then [the handler](#pr-mon-handler).

## How it works

```
 tap ──▶ Telegram ──HTTPS──▶ telegram-webhook (public Function URL)
                               │ 1. secret header matches?    else 401
                               │ 2. sender on the allow list? else dropped
                               │ 3. route = callback data up to the first ":"
                               ▼
              ┌────────────────┴────────────────┐
   telegram-webhook-pr-mon.fifo     telegram-webhook-backup.fifo     ...one per route
              ▲                                  ▲
        pr-mon long-polls                  backup job long-polls
        (its own reader key)               (its own reader key)
```

A button sent with callback data `pr-mon:k7` lands on the `pr-mon` queue.
Taps whose prefix matches no route are answered "Nothing handles this button"
and dropped.

The receiver answers every routed tap itself ("Sent to pr-mon") by returning
`answerCallbackQuery` in its HTTP response, which Telegram performs for it. So
the button stops spinning even when the consumer is offline, and consumers
report results by editing the message, not by answering the tap.

## Security

| Layer | What it stops |
|:------|:--------------|
| Secret header | Anyone but Telegram. Telegram sends the secret registered with `setWebhook` in `X-Telegram-Bot-Api-Secret-Token`; anything else gets 401. Until the secret is seeded, every call gets 503. |
| Allow list | Anyone but you. Anyone can tap a bot's buttons; taps from other user ids are dropped silently (200, so Telegram does not retry). |
| Reserved concurrency | Floods. At most 5 copies of the receiver run at once. |
| Per-route reader keys | Apps reading each other's taps. Each key can receive and delete on one queue, and invoke the sender, and nothing else. |
| Short retention | Stale taps. An unread tap expires after 10 minutes, so a Merge tapped while the Mac slept doesn't fire an hour later. |

The receiver never holds the bot token. Only the sender and register functions
read it, and neither has a URL: they run only when invoked with IAM
credentials.

## What Terraform creates

| Resource | Purpose |
|:---------|:--------|
| `telegram-webhook` function + Function URL | The public receiver |
| `telegram-webhook-sender` function | Sends, edits and deletes messages for the readers |
| `telegram-webhook-register` function | Sets, shows or removes the bot's webhook |
| SQS FIFO queue per route | FIFO so a tap Telegram redelivers (same `update_id`) is queued once |
| SSM parameters | The bot token and the webhook secret (SecureString, seeded by hand) |
| IAM user + access key per route | The consumer's credential: receive/delete on its queue, invoke the sender |
| Roles, log groups | Least privilege: the receiver can read the secret and send to the queues; the sender can read the token; register can read both |

## Setup

```bash
cp terraform.tfvars.example terraform.tfvars
$EDITOR terraform.tfvars            # profile, your user id, default chat, routes
terraform init
terraform apply
```

Your user id is also the chat id of your private chat with the bot. Send the
bot `/start` once so it is allowed to message you.

### Seed the secrets

Terraform creates both parameters with placeholders and never stores their
values, so neither enters Terraform state:

```bash
aws ssm put-parameter --name /telegram-webhook/bot-token \
    --type SecureString --overwrite --value "<token from @BotFather>"
aws ssm put-parameter --name /telegram-webhook/secret-token \
    --type SecureString --overwrite --value "$(openssl rand -hex 32)"
```

The functions cache both until their next cold start; after changing either,
force one (e.g. `terraform apply -replace=aws_lambda_function.sender`).

### Register the webhook

```bash
aws lambda invoke --function-name telegram-webhook-register \
    --cli-binary-format raw-in-base64-out --payload '{"action":"set"}' /dev/stdout
```

`{"action":"info"}` shows the current state, including Telegram's last
delivery error. `{"action":"delete"}` removes the webhook.

To rotate the secret: seed a new one, force a cold start of both functions
(any configuration change does, e.g. `terraform apply -replace=aws_lambda_function.webhook`
and the same for register), then `set` again.

## Writing a consumer

1. **Send buttons** through the sender with `callback_data` set to
   `<route>:<anything>`. Telegram allows 64 bytes in all, prefix included.
2. **Long-poll** the route's queue with its reader key: `ReceiveMessage` with
   `WaitTimeSeconds` 20, asking for the `SentTimestamp` attribute. Each body
   is the Telegram
   [Update](https://core.telegram.org/bots/api#update) exactly as received.
3. **Skip stale taps** using `SentTimestamp`, if the action is risky.
4. **Delete** each message once handled.
5. **Report** by editing the message the button was on.

The same key may also invoke the sender, so a consumer needs only one
credential. Its input is `{"message", "destination", "reply_markup",
"parse_mode", "message_id", "delete"}`; see `src/sender.py`.

The credentials and queue URL:

```bash
terraform output queue_urls
terraform output reader_access_key_ids
terraform output -json reader_secret_access_keys
```

Trying it from the shell (SQS speaks JSON):

```bash
printf 'user = "%s:%s"\n' "$KEY" "$SECRET" | curl -sS -K - \
    --aws-sigv4 "aws:amz:us-east-1:sqs" \
    -H 'Content-Type: application/x-amz-json-1.0' \
    -H 'X-Amz-Target: AmazonSQS.ReceiveMessage' \
    -d "{\"QueueUrl\":\"$QUEUE_URL\",\"WaitTimeSeconds\":20,\"MessageSystemAttributeNames\":[\"SentTimestamp\"]}" \
    https://sqs.us-east-1.amazonaws.com/
```

## Adding a route

Add it to `routes` in `terraform.tfvars` and `apply`. The receiver picks up
the new route on its next cold start, which the apply causes, since the
function's environment changes.

## Limits

- **Only button taps.** The webhook subscribes to `callback_query` only, so
  text typed to the bot is not delivered. Widening it means adding to
  `ALLOWED_UPDATES` in `src/register.py` and deciding how a typed message
  picks its route.
- **No `getUpdates`.** While a webhook is set, Telegram refuses `getUpdates`
  for this bot, so don't share the bot with anything that polls for updates.
  Sending (the sender, or other tools using the same token) is unaffected.

## Removing it

```bash
aws lambda invoke --function-name telegram-webhook-register \
    --cli-binary-format raw-in-base64-out --payload '{"action":"delete"}' /dev/stdout
terraform destroy
```

Delete the webhook first: destroying the register function first leaves
Telegram calling a URL that no longer exists.

## pr-mon handler

`pr-mon-telegram` is a pr-mon
two-way handler: pr-mon runs it as a long-lived child process and talks to it
in JSON lines on stdin/stdout. The handler does the Telegram work through
the sender and the `pr-mon` queue, so pr-mon knows nothing about Telegram or
AWS.

pr-mon's Homebrew cask installs it with the app, at
`/opt/homebrew/bin/pr-mon-telegram` (inside the bundle at
`PrMon.app/Contents/Resources/bin/`), and upgrades it with pr-mon. From source,
`make install` here builds it to `~/.local/bin/pr-mon-telegram`.

Its settings live in `~/.config/pr-mon/telegram.toml`, which must be
`chmod 600` (the handler refuses to start otherwise):

```toml
region = "us-east-1"
queue_url = "https://sqs.us-east-1.amazonaws.com/<acct>/telegram-webhook-pr-mon.fifo"
sender_function = "telegram-webhook-sender"
chat_id = ""

[aws]
access_key_id = "AKIA..."
secret_access_key = "..."
```

- `queue_url`: the `pr-mon` entry of `terraform output queue_urls`.
- `sender_function`: `terraform output sender_function_name`.
- `access_key_id`: the `pr-mon` entry of `terraform output reader_access_key_ids`.
- `secret_access_key`: the `pr-mon` entry of
  `terraform output -json reader_secret_access_keys`.
- `chat_id`: optional; leave it empty to use `default_chat_id` from
  `terraform.tfvars`.

In pr-mon's config, point `[two_way] command` at it. Use absolute paths:
launchd gives pr-mon no shell environment, so `~` and `$PATH` don't work.

```toml
[two_way]
command = "/opt/homebrew/bin/pr-mon-telegram --config /Users/<you>/.config/pr-mon/telegram.toml"
```

## Tests

```bash
make test                                     # the Lambdas, from this directory
make test                                     # the handler, from the repo root (with all of pr-mon)
```

No network, no AWS: SQS, SSM and the Telegram HTTP calls are all stubbed.
