# Two-Way Notifications Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** pr-mon sends Telegram messages with buttons when a PR changes state, and acts on the taps, through a generic two-way channel interface.

**Architecture:** A `twoway.Channel` interface (post, edit, delete, receive taps, ack) with a Telegram implementation that talks only to AWS (SigV4-signed Lambda invokes to `telegram-sender`, SQS long-polls of the pr-mon queue). A manager inside `internal/service` owns everything PR-specific: which buttons, single-use keys, message state in `state.json`, tap handling through the same code as the dashboard.

**Tech Stack:** Go standard library (net/http, crypto/hmac, crypto/sha256), BurntSushi/toml, Bubble Tea (TUI), SwiftUI (Mac app); Python + Terraform for the AWS side.

**Spec:** `docs/superpowers/specs/2026-10-22-two-way-notifications-design.md`

## Global Constraints

- No new Go dependencies (`CLAUDE.md`: ask before adding). SigV4 is standard library only.
- Wire changes are additions only: `protocol.Version` stays as it is. Every wire change also updates `docs/protocol.md`, `make fixtures`, `macos/PrMonKit/Sources/PrMonKit/Models.swift`.
- `make test` (race detector) and `make lint` stay clean; Go tests use standard `testing`, table-driven where it helps, fakes in `internal/testfixtures`.
- Button table (deliberate subset): `READY` → Merge (one per method) + Dismiss; `FAILING` → Re-run failed checks + Force merge (confirm first) + Dismiss; `MERGED` → not sent; every other event → Dismiss. Every message also gets an **Open** URL button.
- AWS secrets live only in `~/.config/pr-mon/credentials.toml` (mode 0600) and never reach the socket.
- The Lambdas learn nothing about PRs.
- Telegram limits: `callback_data` ≤ 64 bytes; text ≤ 4096 chars; bots can only delete messages < 48 h old.
- The backend exits 2 minutes after the last client disconnects unless started at login (`KeepRunning`). Two-way taps need "start at login" on; this plan does not change that.

## Review Focus

1. **Double tap** on the same button (or two buttons of one message) before the first finishes: the second must find its key already retired, never run a second merge. → Task 8, `TestTapTwiceRunsOnce`.
2. **PR title with HTML** (`<`, `>`, `&`, quotes) must be escaped in the Telegram HTML, or Telegram rejects the whole message. → Task 3, `TestRenderEscapesHTML`.
3. **Settings saved from the dashboard** must keep the hand-written `[two_way]` table. → Task 4, `TestSaveKeepsTwoWay`.
4. **Status changed while the backend was down**: the first load after restart (which never notifies) must still delete stale messages. → Task 7, `TestFirstLoadDeletesMovedMessages`.
5. **Relay failure while posting** (Lambda error, network): no message recorded, a toast, and the PR's previous message left alone. → Task 7, `TestPostFailureKeepsPreviousMessage`.

---

### Task 1: `telegram-sender` can delete messages (AWS side, `~/src/telegram-sender`)

Not a git repo: no commits; deploy with a reviewed plan file.

**Files:**
- Modify: `~/src/telegram-sender/src/handler.py`, `~/src/telegram-sender/tests/test_handler.py`, `~/src/telegram-sender/README.md`

**Interfaces:**
- Produces: event `{"delete": true, "message_id": N, "destination"?: str}` → Telegram `deleteMessage`; returns `{"ok": true, "message_id": N, "destination": ...}`. Telegram refusals raise `SendError` whose text contains Telegram's description (e.g. `message can't be deleted`, `message to delete not found`).

- [ ] **Step 1: Write failing tests** in `tests/test_handler.py`:
  - `test_delete_calls_delete_message`: event `{"delete": True, "message_id": 1068}` → request URL ends `/deleteMessage`, form has `chat_id=default-chat`, `message_id=1068`, no `text`; result `{"ok": True, "message_id": 1068, "destination": "default-chat"}`.
  - `test_delete_needs_message_id`: `{"delete": True}` raises `SendError`, `urlopen` not called.
  - `test_delete_ignores_message`: `{"delete": True, "message_id": 5, "message": "x"}` sends no `text`.
  - `test_delete_refusal_surfaces_telegram_text`: HTTP 400 body `{"description":"Bad Request: message can't be deleted for everyone"}` → `SendError` containing `message can't be deleted`.
- [ ] **Step 2:** `uv run python -m unittest discover -s tests` → the four fail.
- [ ] **Step 3:** In `lambda_handler`, when `event.get("delete") is True`: validate `message_id` (int, not bool), resolve destination, POST `deleteMessage` with `chat_id`, `message_id`; skip the `message` checks. Reuse the existing request/error code (factor `call(method, fields)` out of `send`).
- [ ] **Step 4:** Tests pass (19).
- [ ] **Step 5:** README "Event contract": document the delete form.
- [ ] **Step 6:** `terraform plan -out=delete.tfplan` → only `aws_lambda_function.handler` (and the known no-op `aws_iam_user_policy.invoke` re-read); `terraform apply delete.tfplan`; `im --deliver tgram "sender: delete support deployed"` still delivers.

### Task 2: the pr-mon reader key may invoke `telegram-sender` (AWS side, `~/src/telegram-webhook`)

**Files:**
- Modify: `~/src/telegram-webhook/variables.tf`, `main.tf`, `README.md`

- [ ] **Step 1:** Add `variable "sender_function_name"` (string, default `"telegram-sender"`, description: "Lambda each route's reader may invoke to send and edit messages").
- [ ] **Step 2:** In `data "aws_iam_policy_document" "reader"`, add statement `InvokeSender`: `lambda:InvokeFunction` on `arn:aws:lambda:${var.aws_region}:${local.account_id}:function:${var.sender_function_name}`.
- [ ] **Step 3:** README: consumers' key can also invoke the sender; rename the "Per-route reader keys" security row to say so.
- [ ] **Step 4:** `terraform fmt -check && terraform validate`; `terraform plan -out=sender.tfplan` → exactly `aws_iam_user_policy.reader["pr-mon"]` and `["test"]` change; apply.
- [ ] **Step 5:** Verify with the pr-mon key (from `terraform output`): a SigV4 `curl` invoke of `telegram-sender` with `{"message":"pr-mon key can send"}` returns `"ok": true`.

### Task 3: AWS client and Telegram channel (`internal/aws`, `internal/twoway`)

**Files:**
- Create: `internal/aws/sigv4.go`, `internal/aws/client.go`, `internal/aws/aws_test.go`
- Create: `internal/twoway/twoway.go`
- Create: `internal/twoway/telegram/telegram.go`, `internal/twoway/telegram/telegram_test.go`
- Modify: `internal/testfixtures/testfixtures.go` (fake channel)

**Interfaces:**
- Produces (`internal/aws`):
  - `type Credentials struct{ AccessKeyID, SecretAccessKey string }`
  - `func Sign(request *http.Request, body []byte, credentials Credentials, region, service string, now time.Time)`: sets `X-Amz-Date` and `Authorization`; signs `host`, `x-amz-date` and every other header on the request except `User-Agent` and `Authorization`.
  - `type Client struct { Credentials Credentials; Region string; HTTP *http.Client; LambdaEndpoint, SQSEndpoint string; Now func() time.Time }`. Empty endpoints default to `https://lambda.<region>.amazonaws.com` and `https://sqs.<region>.amazonaws.com`. Nil `HTTP` defaults to a client with a 30 s timeout (longer than the 20 s long poll).
  - `func (c *Client) Invoke(ctx context.Context, function string, payload any) (json.RawMessage, error)`: `POST {LambdaEndpoint}/2015-03-31/functions/{function}/invocations`. An `X-Amz-Function-Error` header becomes `*FunctionError{Message}` from the body's `errorMessage`.
  - `type FunctionError struct{ Message string }` (implements `error`)
  - `type SQSMessage struct{ ReceiptHandle, Body string }`
  - `func (c *Client) Receive(ctx context.Context, queueURL string, wait time.Duration, max int) ([]SQSMessage, error)`: JSON protocol (`X-Amz-Target: AmazonSQS.ReceiveMessage`, `Content-Type: application/x-amz-json-1.0`).
  - `func (c *Client) DeleteMessage(ctx context.Context, queueURL, receipt string) error`
- Produces (`internal/twoway`):
  - `var ErrTooOld`, `var ErrGone` (sentinel errors: the service refuses to delete an old message; the message no longer exists)
  - `type Button struct{ Label, Key, URL string }` (a `URL` button sends no tap)
  - `type Message struct{ Status, Title string; Lines []string; Buttons [][]Button }`
  - `type Tap struct{ MessageID, Key, Receipt string }`
  - `type Channel interface { Name() string; Post(ctx context.Context, message Message) (string, error); Edit(ctx context.Context, id string, message Message) error; Delete(ctx context.Context, id string) error; Receive(ctx context.Context) ([]Tap, error); Ack(ctx context.Context, tap Tap) error }`
- Produces (`internal/twoway/telegram`): `type Settings struct{ QueueURL, SenderFunction, Route, ChatID string }` and `func New(settings Settings, client *aws.Client) *Channel` (`*Channel` implements `twoway.Channel`; `Name()` is `"Telegram"`).
- Produces (`internal/testfixtures`): `type TwoWay struct` implementing `twoway.Channel`, recording `Posts []twoway.Message`, `Edits map[string]twoway.Message`, `Deletes []string`, with settable `PostErr`, `DeleteErr error` and a `Taps chan twoway.Tap`; `Receive` blocks until a tap arrives (returning it) or the context ends (returning nil, ctx.Err()), so a receive loop never spins; `Acked []twoway.Tap`. IDs are `"1"`, `"2"`, … in post order. Safe for concurrent use.

- [ ] **Step 1: Failing tests** in `internal/aws/aws_test.go`:
  - `TestSignGetVanilla`: `GET https://example.amazonaws.com/`, credentials `AKIDEXAMPLE` / `wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY`, region `us-east-1`, service `service`, `now` = 2015-08-30 12:36:00 UTC → `X-Amz-Date: 20150830T123600Z`, `Authorization: AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31`.
  - `TestSignSQSPost`: `POST https://sqs.us-east-1.amazonaws.com/`, headers `Content-Type: application/x-amz-json-1.0`, `X-Amz-Target: AmazonSQS.ReceiveMessage`, body `{"QueueUrl":"https://sqs.us-east-1.amazonaws.com/123456789012/q.fifo","WaitTimeSeconds":20}`, same credentials and time, service `sqs` → `SignedHeaders=content-type;host;x-amz-date;x-amz-target, Signature=82fe94d3b04c19ba3416d775c25ab6e17e12b6ea2702632e5728e948e26c851c` (computed with botocore).
  - `TestInvokeReturnsPayload`, `TestInvokeFunctionError` (header `X-Amz-Function-Error: Unhandled`, body `{"errorMessage":"boom"}` → `*FunctionError{"boom"}`), `TestReceiveAndDelete` (httptest server asserts target header, request JSON, returns `{"Messages":[{"ReceiptHandle":"r1","Body":"b"}]}`; empty object → no messages).
- [ ] **Step 2:** `go test ./internal/aws/` → fails (no package).
- [ ] **Step 3:** Implement `internal/aws`. Canonical request: method, URI-encoded path (`/` when empty), canonical query, lowercased sorted headers `name:trimmed value\n`, signed header list, hex SHA-256 of body; string to sign `AWS4-HMAC-SHA256\n<amzdate>\n<date>/<region>/<service>/aws4_request\n<hex sha256 canonical>`; key chain `HMAC("AWS4"+secret, date) → region → service → "aws4_request"`.
- [ ] **Step 4:** `go test ./internal/aws/` passes.
- [ ] **Step 5: Failing tests** in `internal/twoway/telegram/telegram_test.go` (httptest servers standing in for Lambda and SQS; capture invoke payloads):
  - `TestPostSendsHTMLWithKeyboard`: `Post(Message{Status:"READY", Title:"acme/api #123 · Fix", Lines:["by alice"], Buttons:[[{Label:"Squash & merge",Key:"k1"},{Label:"Dismiss",Key:"k2"}],[{Label:"Open",URL:"https://github.com/acme/api/pull/123"}]]})` → payload has `parse_mode:"HTML"`, `reply_markup.inline_keyboard` with `callback_data:"pr-mon:k1"`, `"pr-mon:k2"` and a `url` button; returns `"1069"` from sender's `{"ok":true,"message_id":1069}`. `destination` present only when `ChatID` set.
  - `TestRenderEscapesHTML`: title `a <b> & "c"` appears as `a &lt;b&gt; &amp; &#34;c&#34;` (or `&quot;`; `html.EscapeString` output).
  - `TestEditSendsMessageID` (no `reply_markup` when `Buttons` empty), `TestEditNotModifiedIsSuccess` (function error containing `message is not modified` → nil).
  - `TestDeleteMapsTelegramRefusals`: function error containing `message can't be deleted` → `ErrTooOld`; `message to delete not found` → `ErrGone`; `message to edit not found` (from Edit) → `ErrGone`.
  - `TestReceiveParsesTaps`: SQS body = Telegram update `{"update_id":1,"callback_query":{"id":"c","data":"pr-mon:k1","message":{"message_id":1069}}}` → `Tap{MessageID:"1069", Key:"k1", Receipt:"r1"}`; a body without the prefix or unparseable → skipped *and* deleted from the queue.
  - `TestCallbackDataFits`: with route `pr-mon` and key `abcdefgh`, the encoded `callback_data` is `pr-mon:abcdefgh` (15 bytes, within Telegram's 64).
- [ ] **Step 6:** Tests fail.
- [ ] **Step 7:** Implement `internal/twoway/twoway.go` and `internal/twoway/telegram/telegram.go`. Layout (*builder's call* on emoji): first line `<b>{emoji} {Status}</b>`, then `<b>{Title}</b>`, then each line. Keep the text within 4096 characters by shortening the plain text of `Title` and `Lines` *before* escaping, so no markup or entity is ever cut. Telegram descriptions are matched by substring (strings above are from memory; Task 10 confirms them live).
- [ ] **Step 8:** Add the fake `testfixtures.TwoWay`.
- [ ] **Step 9:** `go test ./internal/aws/ ./internal/twoway/... ./internal/testfixtures/` passes; `make lint` clean.
- [ ] **Step 10: Commit** `git add internal/aws internal/twoway internal/testfixtures && git commit -m "Add a two-way channel interface and a Telegram channel over AWS"`

### Task 4: Config: `[two_way]`, `two_way_enabled`, credentials

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Create: `internal/config/credentials.go`, `internal/config/credentials_test.go`

**Interfaces:**
- Produces:
  - `type TwoWayConfig struct { Kind, Region, QueueURL, SenderFunction, Route, ChatID string }` with TOML tags `kind`, `region`, `queue_url`, `sender_function`, `route`, `chat_id`.
  - `func (t TwoWayConfig) Problem() string`: `""` when usable; otherwise a sentence. Rules: `kind` must be `"telegram"`; `region`, `queue_url`, `sender_function`, `route` non-empty; `route` matches `^[a-z0-9_-]{1,20}$`.
  - `Config.TwoWay *TwoWayConfig` with `json:"-"` (never on the wire).
  - `NotifyConfig.TwoWayEnabled bool` with `json:"two_way_enabled" toml:"two_way_enabled"`; included in `Equal` and `Save`.
  - `type Credentials struct{ AWS *AWSCredentials }`, `type AWSCredentials struct{ AccessKeyID, SecretAccessKey string }` (TOML `[aws]` `access_key_id`, `secret_access_key`)
  - `func CredentialsPath(configPath string) string` → `filepath.Join(filepath.Dir(configPath), "credentials.toml")`
  - `func LoadCredentials(path string) (Credentials, error)`: error when missing, unreadable, invalid TOML, or `mode & 0o077 != 0` ("credentials.toml must be readable only by you (chmod 600)").

- [ ] **Step 1: Failing tests:**
  - `TestTwoWayRoundTrip`: load a file with `[two_way]` (all six keys) → `Config.TwoWay` equals the struct; `Save` then `Load` → same.
  - `TestSaveKeepsTwoWay`: load with `[two_way]`, change `PollInterval`, `Save`, reload → `TwoWay` unchanged.
  - `TestNoTwoWayMeansNil`: file without the table → `TwoWay == nil`, and `Save` writes no `[two_way]`.
  - `TestTwoWayProblem` (table): each rule above, plus a valid config → `""`.
  - `TestTwoWayEnabledRoundTrip`: `two_way_enabled = true` survives load/save; missing key → false; `Equal` sees a difference.
  - `TestTwoWayNotInJSON`: `json.Marshal(config)` contains no `two_way` key at top level.
  - Credentials: `TestLoadCredentials` (0600 file → values), `TestCredentialsTooOpen` (0644 → error mentioning `chmod 600`), `TestCredentialsMissing`.
- [ ] **Step 2:** `go test ./internal/config/` → fail.
- [ ] **Step 3:** Implement. Parse `[two_way]` via a `TwoWay *TwoWayConfig` field on `raw` (an invalid table is kept as written, so `Save` doesn't destroy it; the service reports `Problem()`). `Save` writes `[two_way]` after `repos` and before the notification tables, with `tomlValue` for every value.
- [ ] **Step 4:** Tests pass; `make lint`.
- [ ] **Step 5: Commit** `git commit -m "Read and keep a [two_way] config table and a credentials file"`

### Task 5: State: tracked messages

**Files:**
- Modify: `internal/state/state.go`, `internal/state/state_test.go`

**Interfaces:**
- Produces:
  - `type Message struct { Repo string; Number int; Event, Status, HeadSha, SentAt string; Keys map[string]Button; Confirming bool }` (JSON: `repo`, `number`, `event`, `status`, `head_sha`, `sent_at`, `keys`, `confirming`)
  - `type Button struct { Kind string; Method string; DeleteBranch bool }` (JSON `kind`, `method` omitempty, `delete_branch` omitempty). Kinds: `merge`, `force_merge` (asks to confirm), `confirm_force_merge`, `cancel`, `rerun_checks`, `dismiss`.
  - `AppState.Messages map[string]Message` (`json:"messages"`, keyed by channel message ID); `New` and `Save` default it to `{}`; `Load` reads it on its own like the other sections.

- [ ] **Step 1: Failing tests:** `TestMessagesRoundTrip`; `TestBadMessagesSectionKeepsTheRest` (`"messages": 7` → empty messages, other sections loaded); `TestOldStateHasNoMessages` (file without the key → empty map, not nil).
- [ ] **Step 2:** `go test ./internal/state/` → fail. **Step 3:** implement. **Step 4:** pass.
- [ ] **Step 5: Commit** `git commit -m "Remember two-way messages in state.json"`

### Task 6: `perform`: report the outcome and pin the head

**Files:**
- Modify: `internal/service/commands.go`, `internal/service/service_test.go`

**Interfaces:**
- Produces: `func (m *Monitor) perform(ctx context.Context, repoName string, number int, action models.Action, expectedHead string) error`: everything `Perform` does today, but returns any failure (validation or GitHub) instead of toasting it; success toasts stay. `runAction` gains `expectedHead string` and passes it to `Merge` for `merge` and `force_merge`. `Perform` becomes `perform(..., "")` plus today's failure toast for GitHub errors, so its behavior is unchanged.

- [ ] **Step 1: Failing tests:** `TestPerformPinnedPassesExpectedHead` (fake GitHub records `expectedHeadOid` = `"abc"` for `merge` and `force_merge`); `TestPerformReturnsGitHubFailure` (fake `Merge` error → `perform` returns it; no error toast emitted by `perform`).
- [ ] **Step 2:** fail. **Step 3:** implement. **Step 4:** `go test ./internal/service/` passes, including every existing `Perform` test unchanged.
- [ ] **Step 5: Commit** `git commit -m "Let callers learn an action's outcome and pin a merge to a head commit"`

### Task 7: Two-way manager: sending, superseding, cleanup

**Files:**
- Create: `internal/service/twoway.go`, `internal/service/twoway_test.go`
- Modify: `internal/service/service.go` (Options, Status, Start, `apply`, `notifyResult`, `send`'s "Nothing to send" message), `internal/service/commands.go` (`RemoveRepo`, `SendTest`), `internal/daemon/run.go` only if the default builder needs wiring

**Interfaces:**
- Consumes: Tasks 3–6.
- Produces:
  - `service.Options.TwoWay func(config.TwoWayConfig, config.Credentials) (twoway.Channel, error)`; nil means the default, which builds `telegram.New` with an `aws.Client`.
  - `Status.TwoWay *string` (`json:"two_way"`): the channel's `Name()` when running.
  - `func buttonsFor(repo models.Repo, pr models.PullRequest, event string) []state.Button` (pure): the table in Global Constraints. Merge buttons come from the PR's `Available` option with `Kind == "merge"`, one per `repo.MergeMethods`, `DeleteBranch` = the option's `OffersDeleteBranch`. Force merge from an `Available` option with `Kind == "force_merge"` (its `Method` = first of `repo.MergeMethods`). Re-run from an `Available` `rerun_checks` option. Dismiss always.
  - Labels (*my call*): merge methods `Squash & merge`, `Merge commit`, `Rebase & merge`; `Re-run failed checks`; `Force merge`; `Dismiss`; URL button `Open`.

Behavior (from the spec):
- **Start:** when `config.TwoWay` is set: `Problem()` non-empty, or credentials fail to load (or have no `[aws]` section), or the builder errors → a status warning `Two-way notifications are off: <reason>` and nothing else. Otherwise set `Status.TwoWay` and spawn the receive loop (Task 8).
- **After `apply`** (outside the lock, background like `send`), for a repo whose `two_way_enabled` is on, not on first load: for each selected notification that is not `MERGED` and not muted, *post first, then* delete the PR's previous message, then record the new one (status, head SHA, event, keys, `SentAt`). On every apply, including first load: for each tracked message of the repo, if the PR is gone and the list isn't truncated (`PRTotal <= len(PRs)`) → retire with `PR closed`; else if its status differs from the recorded one and no new message is being sent for it → retire with `Status changed`.
- **`notifyResult`:** `MERGE_FAILED` posts too, same rules.
- **`RemoveRepo`:** retire every message of that repo with `Repo removed`.
- **`SendTest`:** when the settings have `TwoWayEnabled` and a channel runs, post a sample (`Status: "TEST"`, Dismiss only), tracked with `Number: 0`; cleanup skips `Number: 0`. The "Nothing to send" warning reads `Nothing to send: enable Script, Desktop or Two-way notifications`.
- **Retire** `(id, why)`: `Delete`; on `ErrTooOld` → `Edit` with the message's text, no buttons, plus the `why` line; on `ErrGone` → nothing; then forget the message. Errors are logged and toasted (`Telegram notification failed: …`).
- **Keys:** 8 characters from `crypto/rand`, base-32 lowercase.

- [ ] **Step 1: Failing tests** (fake GitHub + `testfixtures.TwoWay`, via the existing test harness):
  - `TestButtonsFor` (table): READY with methods [SQUASH] → `merge/SQUASH` + `dismiss`; READY with all three methods → three merges + dismiss; FAILING with a failed Actions check and `CanBypass` → `rerun_checks`, `force_merge`, `dismiss`; FAILING without bypass → `rerun_checks`, `dismiss`; BLOCKED → `dismiss` only (even with `CanBypass`); CONFLICT → `dismiss`.
  - `TestPostsOnSelectedEvent`: repo with `two_way_enabled`, PR moves PENDING → READY → one post, message tracked with status READY and head SHA.
  - `TestMergedIsNotSent`, `TestMutedIsNotSent`, `TestDisabledRepoIsNotSent`.
  - `TestNewEventSupersedes`: READY then FAILING → second post, first message deleted.
  - `TestStatusMovedDeletes`: READY message; PR goes CONFLICT with CONFLICT not selected → deleted, nothing posted.
  - `TestFirstLoadDeletesMovedMessages`: saved state has a READY message; fresh monitor's first load sees the PR FAILING → deleted, nothing posted.
  - `TestClosedPRDeletes` and `TestTruncatedListKeepsMessage`.
  - `TestTooOldFallsBackToEdit`: `DeleteErr = ErrTooOld` → an edit with no buttons containing `PR closed`; message forgotten.
  - `TestPostFailureKeepsPreviousMessage`: `PostErr` set on the second event → first message still tracked, not deleted; a warning toast.
  - `TestRemoveRepoDeletesMessages`, `TestSendTestPostsSample`, `TestMergeFailedPosts`.
  - `TestStartWarnsOnBadSetup`: `[two_way]` with an empty `route` → status warning, `Status.TwoWay == nil`.
- [ ] **Step 2:** `go test ./internal/service/ -run 'TwoWay|Buttons|Posts|Supersede|Moved|Closed|Truncated|TooOld|PostFailure|RemoveRepo|SendTest|MergeFailed|StartWarns'` → fail.
- [ ] **Step 3:** Implement `internal/service/twoway.go` and the hooks.
- [ ] **Step 4:** `make test` passes; `make lint` clean.
- [ ] **Step 5: Commit** `git commit -m "Send two-way notifications and clean them up"`

### Task 8: Two-way manager: taps

**Files:**
- Create: `internal/service/taps.go`, `internal/service/taps_test.go`

**Interfaces:**
- Consumes: Tasks 5–7.
- Produces: `func (m *Monitor) receiveLoop()` (spawned by Start) and `func (m *Monitor) handleTap(ctx context.Context, tap twoway.Tap)`.

Behavior (from the spec), every tap acknowledged afterwards whatever happens:
1. Under the lock, find the message and the key; **retire all of that message's keys before doing anything slow**, so a second tap finds nothing.
2. Unknown message → `Edit` it with no buttons and the line `No longer tracked`. Known message, unknown key → re-render with current buttons and `That button had expired`.
3. `dismiss` → retire with `Dismissed`.
4. `force_merge` → edit to `Force merge #N?` with **Confirm** (`confirm_force_merge`, carrying method and delete-branch) and **Cancel**; `Confirming = true`. `cancel` → re-render the original.
5. `merge`, `confirm_force_merge`, `rerun_checks` → `refreshPRs(ctx, repo, []int{n})`; PR gone → retire with `PR closed`; head SHA differs from the recorded one → re-render with `The PR changed since this message`; the matching option no longer `Available` → re-render with its `Reason` (or `No longer available`); else `perform(ctx, repo, n, action, recordedHead)`. Success → retire with `✅ Done` (merge: `✅ Merged`; re-run: `🔁 Re-running failed checks`). Failure → re-render with `⚠️ <error>`.
- Every re-render issues fresh keys. Receive errors back off 5 s doubling to 1 min and are logged; the loop ends when the monitor's context ends.

- [ ] **Step 1: Failing tests:** `TestTapMergeMergesAndDeletes` (fake GitHub sees `expectedHeadOid` = recorded head, message deleted, tap acked); `TestTapTwiceRunsOnce` (two taps on the same key, handled concurrently → one `Merge` call; the second re-renders or is ignored); `TestTapHeadMovedRefuses`; `TestTapNoLongerAvailable`; `TestTapMergeFailureKeepsMessage` (fake `Merge` error → edit containing the error, fresh keys, message still tracked); `TestForceMergeAsksFirst` then `TestConfirmForceMerge` and `TestCancelRestores`; `TestDismissDeletes`; `TestStaleKey`; `TestUnknownMessageLosesButtons`; `TestRerunChecks`.
- [ ] **Step 2:** fail. **Step 3:** implement. **Step 4:** `make test` passes (race detector covers `TestTapTwiceRunsOnce`).
- [ ] **Step 5: Commit** `git commit -m "Act on two-way taps"`

### Task 9: Protocol, fixtures and both clients

**Files:**
- Modify: `internal/protocol/protocol.go` (`Hello.TwoWay *string` `json:"two_way"`), `internal/server/dispatch.go` (fill it from `Status().TwoWay`), `docs/protocol.md` (rows for `two_way` in Hello, Status, and `two_way_enabled` in `NotifyConfig`), `cmd/gen-fixtures/main.go` (`TwoWay: models.Ptr("Telegram")` in status and hello; `TwoWayEnabled: true` in the sample settings), `protocol-fixtures/*` (regenerated)
- Modify: `internal/tui/modals.go`, `internal/tui/modalview.go`, `internal/tui/keys.go`, TUI tests
- Modify: `macos/PrMonKit/Sources/PrMonKit/Models.swift`, its tests, `macos/PrMon/PrMon/SettingsView.swift`

**Interfaces:**
- Consumes: `Status.TwoWay` (Task 7), `NotifyConfig.TwoWayEnabled` (Task 4).

- [ ] **Step 1:** Protocol: add the fields and doc rows; `go test ./internal/protocol/` (spec test) passes; `make fixtures`; `git diff protocol-fixtures` shows only the new keys.
- [ ] **Step 2: TUI, failing test first:** a notifications-modal test: with `twoWay = "Telegram"`, the "Two-way" tab (after Desktop) toggles `TwoWayEnabled` on space and renders `Send to Telegram with buttons` and `Using Telegram`; with `twoWay == nil`, space does nothing and it renders `No two-way channel set up ([two_way] in config.toml)`. Implement: `tabTwoWay`, `tabNames` gains `"Two-way"`, `newNotifications` gains the `twoWay *string` parameter (from `m.backend.Status().TwoWay` in `keys.go`).
- [ ] **Step 3: Swift, failing test first:** decoding the regenerated fixtures yields `hello.twoWay == "Telegram"`, `status.twoWay == "Telegram"`, `notifyConfig.twoWayEnabled == true`; a `NotifyConfig` JSON without `two_way_enabled` decodes as `false` (`decodeIfPresent`). Implement in `Models.swift` (`twoWay`, `twoWayEnabled`, coding keys `two_way`, `two_way_enabled`).
- [ ] **Step 4: Mac app:** in `SettingsView.editor`, under the desktop toggle: `Toggle("Send to \(twoWay ?? "a chat") with buttons", isOn: settings.twoWayEnabled).disabled(twoWay == nil)`, `twoWay` read like `notifier` (`store.state?.snapshot.status.twoWay`); footer line `Two-way notifications use \(twoWay).` or `No two-way channel set up ([two_way] in config.toml).`; `outgoing` keeps the saved `twoWayEnabled` when `twoWay == nil`.
- [ ] **Step 5:** `make test`, `make lint`, `make swift-test`, `make app` all succeed. Exercise the TUI dialog by hand (tab to Two-way, space, ctrl+s) and the Mac app toggle.
- [ ] **Step 6: Commit** `git commit -m "Show a per-repo two-way switch in the dashboard and the Mac app"`

### Task 10: Set up and check end to end

**Files:**
- Create: `~/.config/pr-mon/credentials.toml` (0600, from `terraform -chdir=~/src/telegram-webhook output` for route `pr-mon`)
- Modify: `~/.config/pr-mon/config.toml` (`[two_way]` with `kind = "telegram"`, `region = "us-east-1"`, `queue_url` = the pr-mon queue, `sender_function = "telegram-sender"`, `route = "pr-mon"`, `chat_id = ""`)

- [ ] **Step 1:** Write both files; `chmod 600 ~/.config/pr-mon/credentials.toml`.
- [ ] **Step 2:** `make install`, restart the backend (`pr-mon restart` or the app's control), check `pr-mon` status shows no two-way warning and Settings shows the switch.
- [ ] **Step 3:** Turn the switch on for one repo with READY selected; trigger a READY (or use **Send test**). On the phone: the message shows formatted with buttons; **Dismiss** removes it.
- [ ] **Step 4:** Confirm the Telegram error texts the channel matches (`message can't be deleted`, `message to delete not found`, `message is not modified`) by provoking each (delete twice; edit with identical content); fix the substrings if Telegram's wording differs, with a test.
- [ ] **Step 5:** Tap a stale button (one from a superseded message) → "No longer tracked" or "expired", nothing runs. Close a PR on GitHub → its message disappears within a poll.
- [ ] **Step 6:** Report results; no commit (files are outside the repo).
