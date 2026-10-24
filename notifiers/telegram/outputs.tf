output "webhook_url" {
  description = "Public URL Telegram calls. The register function sets it; you never need to type it."
  value       = aws_lambda_function_url.webhook.function_url
}

output "register_function_name" {
  description = "Invoke with {\"action\": \"set\" | \"info\" | \"delete\"}."
  value       = aws_lambda_function.register.function_name
}

output "sender_function_name" {
  description = "The handler's sender_function."
  value       = aws_lambda_function.sender.function_name
}

output "region" {
  value = var.aws_region
}

output "queue_urls" {
  description = "Route name -> queue URL. Each consumer long-polls its own."
  value       = { for route, queue in aws_sqs_queue.route : route => queue.url }
}

output "reader_access_key_ids" {
  description = "Route name -> access key id of that route's reader."
  value       = { for route, key in aws_iam_access_key.reader : route => key.id }
}

output "reader_secret_access_keys" {
  description = "Read with: terraform output -json reader_secret_access_keys"
  value       = { for route, key in aws_iam_access_key.reader : route => key.secret }
  sensitive   = true
}
