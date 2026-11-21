output "subnet_ids" {
  description = "VPC subnets carved per IDC role (app/db) plus the global subnet."
  value       = module.network.subnet_ids
}

output "dcn_units" {
  description = "Per-unit app node private IPs and unit DB endpoint (host:port)."
  value = {
    for name, unit in module.dcn_unit : name => {
      app_private_ips = unit.app_private_ips
      db_endpoint     = unit.db_endpoint
    }
  }
}

output "global_zone" {
  description = "Global zone service node IPs, MQ node IP, DB endpoints, and ingress LB id."
  value       = module.global_zone.summary
}

output "expansion_state" {
  description = "Whether the expansion unit (compose profile 'expansion') is provisioned."
  value       = var.resource_plan.expansion.enabled ? "expansion unit ${var.resource_plan.expansion.name} provisioned" : "expansion disabled; set resource_plan.expansion.enabled = true to add the unit"
}

output "topology_file" {
  description = "Absolute path of the rendered topology artifact (see topology.tf)."
  value       = abspath(local_file.dcn_topology.filename)
}

output "dr_replicas" {
  description = "Cross-region DR replica endpoints per unit (empty when DR disabled)."
  value       = var.resource_plan.dr.enabled ? module.dr_replica[0].replica_endpoints : {}
}

output "db_layer" {
  description = "DB middleware layer node inventory (keeper quorum + proxies)."
  value = {
    keeper_private_ips = module.db_layer.keeper_private_ips
    proxy_private_ips  = module.db_layer.proxy_private_ips
  }
}
