output "summary" {
  description = "Global zone endpoints and node inventory."
  value = {
    sf_private_ips   = aws_instance.svc[*].private_ip
    mq_private_ip    = aws_instance.mq.private_ip
    mgmt_private_ips = aws_instance.mgmt[*].private_ip
    db_endpoints     = { for k, db in aws_db_instance.svc_db : k => db.endpoint }
    ingress_lb_dns   = aws_lb.ingress.dns_name
  }
}
