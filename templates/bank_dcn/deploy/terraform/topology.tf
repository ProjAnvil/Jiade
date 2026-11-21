# Topology handoff: renders the applied plan plus outputs into one JSON
# artifact the deployment stage consumes (GNS route seeding, RMB wiring, env
# files). This is infrastructure-level "unit awareness" only — runtime
# awareness (route registration, draining, failover) stays with GNS and the
# deployment stage, exactly like the compose expansion gate.

resource "local_file" "dcn_topology" {
  filename = "${path.module}/generated/dcn-topology.json"

  # jsonencode of the applied plan, per-unit endpoints, and global zone.
  # timestamp() forces a re-render on every apply so the artifact always
  # matches the current state.
  content = jsonencode({
    generated_at = timestamp()
    region       = var.resource_plan.region
    zones        = local.idc_zones
    units = {
      for name, u in merge(local.base_units, local.expansion_units) : name => {
        idc             = u.idc
        primary_zone    = local.idc_zones["idc${u.idc}"]
        app_private_ips = module.dcn_unit[name].app_private_ips
        db_endpoint     = module.dcn_unit[name].db_endpoint
      }
    }
    global_zone = module.global_zone.summary
  })

  file_permission = "0644"
}
