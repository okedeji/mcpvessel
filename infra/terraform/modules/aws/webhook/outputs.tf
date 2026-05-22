output "instance_id" {
  value = aws_instance.webhook.id
}

output "private_ip" {
  value = aws_instance.webhook.private_ip
}

output "public_ip" {
  value = aws_instance.webhook.public_ip
}

output "endpoint" {
  description = "Webhook URL for agentcage llm.endpoint config"
  value       = "http://${aws_instance.webhook.private_ip}:${var.port}/llm"
}

output "judge_endpoint" {
  description = "Webhook URL for agentcage judge.endpoint config"
  value       = "http://${aws_instance.webhook.private_ip}:${var.port}/judge"
}
