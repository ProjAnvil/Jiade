output "replica_endpoints" {
  description = "DR replica endpoints (host:port) per unit."
  value       = { for k, r in aws_db_instance.replica : k => r.endpoint }
}
