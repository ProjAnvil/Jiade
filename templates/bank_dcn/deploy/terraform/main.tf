# Provisioning layer: maps the planning data onto network + dcn-unit +
# global-zone modules. Unit app tiers alternate across idc1/idc2
# (dcn01->idc1, dcn02->idc2, dcn03->idc1, expansion dcn04->idc2), matching
# the compose topology; unit DBs are Multi-AZ with AWS-managed standby AZs.
# Security zones (dmz/sf/core/mgmt) mirror the compose network redesign.

locals {
  idc_zones = {
    idc1 = var.resource_plan.zones[0]
    idc2 = var.resource_plan.zones[1]
  }

  base_units = { for d in var.resource_plan.dcns : d.name => d }

  expansion_units = var.resource_plan.expansion.enabled ? {
    (var.resource_plan.expansion.name) = {
      idc       = var.resource_plan.expansion.idc
      app_nodes = var.resource_plan.expansion.app_nodes
    }
  } : {}

  # RDS subnet groups need subnets in at least two AZs.
  db_subnet_ids = [
    module.network.subnet_ids["idc1-db"],
    module.network.subnet_ids["idc2-db"],
  ]

  # Cross-region read replicas require automated backups on the source,
  # so DR-enabled plans force at least one backup day on unit DBs.
  unit_backup_days = var.resource_plan.dr.enabled ? max(var.db_backup_retention_days, 1) : var.db_backup_retention_days
}

module "network" {
  source = "./modules/network"

  prefix        = var.prefix
  vpc_cidr      = var.resource_plan.cidr
  idc_zones     = local.idc_zones
  admin_cidr    = var.admin_cidr
  resource_tags = var.resource_tags
}

module "dcn_unit" {
  for_each = merge(local.base_units, local.expansion_units)

  source = "./modules/dcn-unit"

  prefix                   = "${var.prefix}-${each.key}"
  primary_zone             = local.idc_zones["idc${each.value.idc}"]
  app_nodes                = each.value.app_nodes
  app_instance_type        = var.app_instance_type
  image_id                 = var.image_id
  db_instance_class        = var.db_instance_class
  db_allocated_gb          = var.db_allocated_gb
  db_multi_az              = var.db_multi_az
  db_read_replica          = var.db_read_replica
  db_backup_retention_days = local.unit_backup_days
  db_master_password       = var.db_master_password
  resource_tags            = var.resource_tags

  vpc_id        = module.network.vpc_id
  app_subnet_id = module.network.subnet_ids["idc${each.value.idc}-app"]
  db_subnet_ids = local.db_subnet_ids
  sg_sf_id      = module.network.security_group_ids["sf"]
  sg_core_id    = module.network.security_group_ids["core"]
  sg_mgmt_id    = module.network.security_group_ids["mgmt"]
}

module "global_zone" {
  source = "./modules/global-zone"

  prefix                   = "${var.prefix}-global"
  zone                     = local.idc_zones["idc1"]
  service_nodes            = var.resource_plan.global_zone.service_nodes
  mgmt_nodes               = var.resource_plan.global_zone.mgmt_nodes
  db_services              = ["gns", "rmb", "adm", "batch"] # one MySQL each, compose parity
  db_instance_class        = var.db_instance_class
  db_allocated_gb          = var.db_allocated_gb
  db_backup_retention_days = var.db_backup_retention_days
  app_instance_type        = var.app_instance_type
  image_id                 = var.image_id
  db_master_password       = var.db_master_password
  resource_tags            = var.resource_tags

  vpc_id         = module.network.vpc_id
  sf_subnet_id   = module.network.subnet_ids["sf"]
  mgmt_subnet_id = module.network.subnet_ids["mgmt"]
  dmz_subnet_ids = [
    module.network.subnet_ids["dmz-a"],
    module.network.subnet_ids["dmz-b"],
  ]
  db_subnet_ids = local.db_subnet_ids
  sg_dmz_id     = module.network.security_group_ids["dmz"]
  sg_sf_id      = module.network.security_group_ids["sf"]
  sg_core_id    = module.network.security_group_ids["core"]
  sg_mgmt_id    = module.network.security_group_ids["mgmt"]
}

# Second city: cross-region async read replicas of every unit DB.
module "dr_replica" {
  source = "./modules/dr-replica"
  count  = var.resource_plan.dr.enabled ? 1 : 0

  providers = { aws = aws.dr }

  prefix            = "${var.prefix}-dr"
  vpc_cidr          = cidrsubnet(var.resource_plan.cidr, 8, 240) # 10.60.240.0/24-style slice of a /16 plan
  zones             = ["${var.resource_plan.dr.region}a", "${var.resource_plan.dr.region}b"]
  db_instance_class = var.db_instance_class
  source_dbs = merge(
    { for name, u in module.dcn_unit : name => u.db_arn },
    { for name, arn in module.global_zone.db_arns : "global-${name}" => arn },
  )
  resource_tags = var.resource_tags
}
