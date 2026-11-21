output "app_private_ips" {
  description = "Private IPs of the unit's app nodes."
  value       = aws_instance.app[*].private_ip
}

output "db_endpoint" {
  description = "Unit DB endpoint (host:port)."
  value       = aws_db_instance.unit.endpoint
}
