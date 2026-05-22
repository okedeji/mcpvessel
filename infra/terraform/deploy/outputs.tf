output "instance_id" {
  value = module.agentcage.instance_id
}

output "public_ip" {
  value = module.agentcage.public_ip
}

output "grpc_addr" {
  value = module.agentcage.grpc_addr
}

output "ssh_command" {
  value = var.enable_ssh ? "ssh -i ${path.module}/agentcage-ssh.pem ubuntu@${module.agentcage.public_ip}" : ""
}

output "connect_command" {
  value = module.agentcage.connect_command
}

output "pause_command" {
  description = "Stop the instance (keeps disk, no compute cost)"
  value       = "aws ec2 stop-instances --instance-ids ${module.agentcage.instance_id}"
}

output "resume_command" {
  description = "Start the instance back up"
  value       = "aws ec2 start-instances --instance-ids ${module.agentcage.instance_id}"
}

output "webhook_endpoint" {
  description = "LLM webhook URL — set this as llm.endpoint in agentcage config"
  value       = module.webhook.endpoint
}

output "webhook_judge_endpoint" {
  description = "Judge webhook URL — set this as judge.endpoint in agentcage config"
  value       = module.webhook.judge_endpoint
}

output "webhook_instance_id" {
  value = module.webhook.instance_id
}

output "webhook_ssh_command" {
  description = "SSH into the webhook EC2 (only when enable_ssh = true)."
  value       = var.enable_ssh ? "ssh -i ${path.module}/agentcage-ssh.pem ec2-user@${module.webhook.public_ip}" : ""
}
