output "app_private_ips" {
  description = "Private IPs of the unit's app nodes."
  value       = aws_instance.app[*].private_ip
}

output "db_endpoint" {
  description = "Unit DB endpoint (host:port)."
  value       = aws_db_instance.unit.endpoint
}

output "db_arn" {
  description = "Unit DB instance ARN (replication source for the DR region)."
  value       = aws_db_instance.unit.arn
}

output "unit_sg_id" {
  description = "Per-unit app security group id (network-enforced unit isolation)."
  value       = aws_security_group.unit.id
}
