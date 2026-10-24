
# Each handler has no third-party dependencies, so each zip is one file.
data "archive_file" "webhook" {
  type        = "zip"
  source_file = "${path.module}/src/webhook.py"
  output_path = "${path.module}/.terraform/webhook.zip"
}

data "archive_file" "register" {
  type        = "zip"
  source_file = "${path.module}/src/register.py"
  output_path = "${path.module}/.terraform/register.zip"
}

data "archive_file" "sender" {
  type        = "zip"
  source_file = "${path.module}/src/sender.py"
  output_path = "${path.module}/.terraform/sender.zip"
}

# --- Secrets ------------------------------------------------------------
# Created with placeholders and then ignored, so the real values never enter
# Terraform state. Seed them once (see the README):
#   aws ssm put-parameter --name /telegram-webhook/secret-token --type SecureString \
#       --overwrite --value "$(openssl rand -hex 32)"
#   aws ssm put-parameter --name /telegram-webhook/bot-token --type SecureString \
#       --overwrite --value "<token from @BotFather>"

resource "aws_ssm_parameter" "bot_token" {
  name        = "/${var.name}/bot-token"
  description = "Telegram bot token from @BotFather. Read by the sender and register functions."
  type        = "SecureString"
  value       = "placeholder-seed-with-aws-ssm-put-parameter"

  lifecycle {
    ignore_changes = [value]
  }
}

resource "aws_ssm_parameter" "secret_token" {
  name        = "/${var.name}/secret-token"
  description = "Secret Telegram sends in X-Telegram-Bot-Api-Secret-Token on every webhook call."
  type        = "SecureString"
  value       = "placeholder-seed-with-aws-ssm-put-parameter"

  lifecycle {
    ignore_changes = [value]
  }
}

# --- Queues -------------------------------------------------------------
# FIFO so a tap Telegram redelivers (same update_id) is queued only once.

resource "aws_sqs_queue" "route" {
  for_each = var.routes

  name                      = "${var.name}-${each.key}.fifo"
  fifo_queue                = true
  message_retention_seconds = var.queue_retention_seconds
  receive_wait_time_seconds = 20
  sqs_managed_sse_enabled   = true
}

# --- Shared IAM pieces --------------------------------------------------

data "aws_iam_policy_document" "assume_role" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

# --- Receiver (public) --------------------------------------------------

resource "aws_cloudwatch_log_group" "webhook" {
  name              = "/aws/lambda/${var.name}"
  retention_in_days = var.log_retention_days
}

resource "aws_iam_role" "webhook" {
  name               = "${var.name}-role"
  assume_role_policy = data.aws_iam_policy_document.assume_role.json
}

data "aws_iam_policy_document" "webhook" {
  statement {
    sid       = "ReadSecret"
    actions   = ["ssm:GetParameter"]
    resources = [aws_ssm_parameter.secret_token.arn]
  }

  # Needed to decrypt the SecureString, and usable for nothing else.
  statement {
    sid       = "DecryptSecureStringViaSSM"
    actions   = ["kms:Decrypt"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["ssm.${var.aws_region}.amazonaws.com"]
    }
  }

  statement {
    sid       = "QueueTaps"
    actions   = ["sqs:SendMessage"]
    resources = [for queue in aws_sqs_queue.route : queue.arn]
  }

  statement {
    sid       = "WriteOwnLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.webhook.arn}:*"]
  }
}

resource "aws_iam_role_policy" "webhook" {
  name   = "${var.name}-policy"
  role   = aws_iam_role.webhook.id
  policy = data.aws_iam_policy_document.webhook.json
}

resource "aws_lambda_function" "webhook" {
  function_name                  = var.name
  role                           = aws_iam_role.webhook.arn
  handler                        = "webhook.lambda_handler"
  runtime                        = "python3.13"
  filename                       = data.archive_file.webhook.output_path
  source_code_hash               = data.archive_file.webhook.output_base64sha256
  timeout                        = 10
  memory_size                    = 128
  reserved_concurrent_executions = var.reserved_concurrency

  environment {
    variables = {
      SECRET_PARAM     = aws_ssm_parameter.secret_token.name
      ALLOWED_USER_IDS = join(",", var.allowed_user_ids)
      ROUTES           = jsonencode({ for route, queue in aws_sqs_queue.route : route => queue.url })
    }
  }

  depends_on = [aws_iam_role_policy.webhook, aws_cloudwatch_log_group.webhook]
}

# Telegram cannot sign AWS requests, so the URL is open; the function itself
# checks the secret header and the sender.
resource "aws_lambda_function_url" "webhook" {
  function_name      = aws_lambda_function.webhook.function_name
  authorization_type = "NONE"
}

# A NONE-auth URL still needs a resource policy granting both actions to
# everyone (required for URLs created since October 2025). The second grant
# is limited to calls that arrive through the URL.
resource "aws_lambda_permission" "url_invoke_url" {
  statement_id           = "PublicInvokeFunctionUrl"
  action                 = "lambda:InvokeFunctionUrl"
  function_name          = aws_lambda_function.webhook.function_name
  principal              = "*"
  function_url_auth_type = "NONE"
}

resource "aws_lambda_permission" "url_invoke" {
  statement_id             = "PublicInvokeViaFunctionUrl"
  action                   = "lambda:InvokeFunction"
  function_name            = aws_lambda_function.webhook.function_name
  principal                = "*"
  invoked_via_function_url = true
}

# --- Register (run by hand, no URL) -------------------------------------

resource "aws_cloudwatch_log_group" "register" {
  name              = "/aws/lambda/${var.name}-register"
  retention_in_days = var.log_retention_days
}

resource "aws_iam_role" "register" {
  name               = "${var.name}-register-role"
  assume_role_policy = data.aws_iam_policy_document.assume_role.json
}

data "aws_iam_policy_document" "register" {
  statement {
    sid       = "ReadTokenAndSecret"
    actions   = ["ssm:GetParameter"]
    resources = [aws_ssm_parameter.bot_token.arn, aws_ssm_parameter.secret_token.arn]
  }

  statement {
    sid       = "DecryptSecureStringViaSSM"
    actions   = ["kms:Decrypt"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["ssm.${var.aws_region}.amazonaws.com"]
    }
  }

  statement {
    sid       = "WriteOwnLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.register.arn}:*"]
  }
}

resource "aws_iam_role_policy" "register" {
  name   = "${var.name}-register-policy"
  role   = aws_iam_role.register.id
  policy = data.aws_iam_policy_document.register.json
}

resource "aws_lambda_function" "register" {
  function_name    = "${var.name}-register"
  role             = aws_iam_role.register.arn
  handler          = "register.lambda_handler"
  runtime          = "python3.13"
  filename         = data.archive_file.register.output_path
  source_code_hash = data.archive_file.register.output_base64sha256
  timeout          = 30
  memory_size      = 128

  environment {
    variables = {
      BOT_TOKEN_PARAM = aws_ssm_parameter.bot_token.name
      SECRET_PARAM    = aws_ssm_parameter.secret_token.name
      WEBHOOK_URL     = aws_lambda_function_url.webhook.function_url
    }
  }

  depends_on = [aws_iam_role_policy.register, aws_cloudwatch_log_group.register]
}

# --- Sender (invoked by the readers, no URL) ----------------------------

resource "aws_cloudwatch_log_group" "sender" {
  name              = "/aws/lambda/${var.name}-sender"
  retention_in_days = var.log_retention_days
}

resource "aws_iam_role" "sender" {
  name               = "${var.name}-sender-role"
  assume_role_policy = data.aws_iam_policy_document.assume_role.json
}

data "aws_iam_policy_document" "sender" {
  statement {
    sid       = "ReadToken"
    actions   = ["ssm:GetParameter"]
    resources = [aws_ssm_parameter.bot_token.arn]
  }

  statement {
    sid       = "DecryptSecureStringViaSSM"
    actions   = ["kms:Decrypt"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["ssm.${var.aws_region}.amazonaws.com"]
    }
  }

  statement {
    sid       = "WriteOwnLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.sender.arn}:*"]
  }
}

resource "aws_iam_role_policy" "sender" {
  name   = "${var.name}-sender-policy"
  role   = aws_iam_role.sender.id
  policy = data.aws_iam_policy_document.sender.json
}

resource "aws_lambda_function" "sender" {
  function_name    = "${var.name}-sender"
  role             = aws_iam_role.sender.arn
  handler          = "sender.lambda_handler"
  runtime          = "python3.13"
  filename         = data.archive_file.sender.output_path
  source_code_hash = data.archive_file.sender.output_base64sha256
  timeout          = 15
  memory_size      = 128

  environment {
    variables = {
      BOT_TOKEN_PARAM = aws_ssm_parameter.bot_token.name
      DEFAULT_CHAT_ID = var.default_chat_id
    }
  }

  depends_on = [aws_iam_role_policy.sender, aws_cloudwatch_log_group.sender]
}

# --- Consumers: one reader per route ------------------------------------
# Static keys rather than an SSO profile, so a long-running consumer does not
# lose access when a session expires. Each can read and delete on its own
# queue and invoke the sender Lambda, and nothing else.

resource "aws_iam_user" "reader" {
  for_each = var.routes
  name     = "${var.name}-${each.key}-reader"
}

data "aws_iam_policy_document" "reader" {
  for_each = var.routes

  statement {
    sid       = "ConsumeOwnQueue"
    actions   = ["sqs:ReceiveMessage", "sqs:DeleteMessage"]
    resources = [aws_sqs_queue.route[each.key].arn]
  }

  statement {
    sid       = "InvokeSender"
    actions   = ["lambda:InvokeFunction"]
    resources = [aws_lambda_function.sender.arn]
  }
}

resource "aws_iam_user_policy" "reader" {
  for_each = var.routes
  name     = "consume-${var.name}-${each.key}"
  user     = aws_iam_user.reader[each.key].name
  policy   = data.aws_iam_policy_document.reader[each.key].json
}

resource "aws_iam_access_key" "reader" {
  for_each = var.routes
  user     = aws_iam_user.reader[each.key].name
}
