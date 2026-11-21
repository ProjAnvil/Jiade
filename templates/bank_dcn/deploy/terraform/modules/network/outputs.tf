output "vpc_id" {
  description = "VPC id."
  value       = aws_vpc.main.id
}

output "subnet_ids" {
  description = "Subnet ids keyed by idc1-app / idc1-db / idc2-app / idc2-db / dmz-a / dmz-b / sf / mgmt."
  value       = { for k, s in aws_subnet.main : k => s.id }
}

output "security_group_ids" {
  description = "Security group ids keyed by dmz / sf / core / mgmt."
  value = {
    dmz  = aws_security_group.dmz.id
    sf   = aws_security_group.sf.id
    core = aws_security_group.core.id
    mgmt = aws_security_group.mgmt.id
  }
}
