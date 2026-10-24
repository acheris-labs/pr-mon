variable "aws_region" {
  description = "Region for the functions, queues and parameters."
  type        = string
  default     = "us-east-1"
}

variable "aws_profile" {
  description = "Named profile in ~/.aws/config used to APPLY this config. Consumers use the per-route reader users created here, not this profile."
  type        = string
  default     = null
}

variable "name" {
  description = "Prefix for every resource: functions, queues, role, users, the secret parameter."
  type        = string
  default     = "telegram-webhook"
}

variable "allowed_user_ids" {
  description = "Telegram user ids whose taps are accepted. Everyone else is dropped. Find yours in the webhook's logs (\"sender not allowed\") after tapping a button."
  type        = list(number)

  validation {
    condition     = length(var.allowed_user_ids) > 0
    error_message = "List at least one Telegram user id, or nothing will ever be queued."
  }
}

variable "routes" {
  description = "One queue per route. A button whose callback data starts with \"<route>:\" lands on that route's queue."
  type        = set(string)

  validation {
    # Lowercase letters, digits, - and _: valid in queue and IAM user names,
    # and never the ":" that ends a route in callback data. Short, because
    # Telegram allows only 64 bytes of callback data in all.
    condition     = alltrue([for route in var.routes : can(regex("^[a-z0-9_-]{1,20}$", route))])
    error_message = "Route names must be 1-20 characters of a-z, 0-9, - or _."
  }
}

variable "default_chat_id" {
  description = "Chat the sender posts to when the caller names none. For your private chat with the bot, it's your Telegram user id."
  type        = string
}

variable "queue_retention_seconds" {
  description = "How long an unread tap waits. Short on purpose: a Merge tapped while the consumer was offline should not run an hour later."
  type        = number
  default     = 600

  validation {
    condition     = var.queue_retention_seconds >= 60 && var.queue_retention_seconds <= 1209600
    error_message = "SQS allows 60 to 1209600 seconds."
  }
}

variable "reserved_concurrency" {
  description = "Most copies of the public receiver that can run at once, so a flood of junk requests cannot run up a bill."
  type        = number
  default     = 5
}

variable "log_retention_days" {
  description = "How long to keep function logs."
  type        = number
  default     = 14
}
