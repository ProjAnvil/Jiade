output "keeper_private_ips" {
  description = "DBKeeper node private IPs (raft quorum)."
  value       = aws_instance.keeper[*].private_ip
}

output "proxy_private_ips" {
  description = "DBProxy node private IPs (connection layer)."
  value       = aws_instance.proxy[*].private_ip
}
