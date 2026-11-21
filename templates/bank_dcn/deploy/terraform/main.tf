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
}

module "network" {
  source = "./modules/network"

  prefix     = var.prefix
  vpc_cidr   = var.resource_plan.cidr
  idc_zones  = local.idc_zones
  admin_cidr = var.admin_cidr
}

module "dcn_unit" {
  for_each = merge(local.base_units, local.expansion_units)

  source = "./modules/dcn-unit"

  prefix             = "${var.prefix}-${each.key}"
  primary_zone       = local.idc_zones["idc${each.value.idc}"]
  app_nodes          = each.value.app_nodes
  app_instance_type  = var.app_instance_type
  image_id           = var.image_id
  db_instance_class  = var.db_instance_class
  db_allocated_gb    = var.db_allocated_gb
  db_multi_az        = var.db_multi_az
  db_master_password = var.db_master_password

  vpc_id        = module.network.vpc_id
  app_subnet_id = module.network.subnet_ids["idc${each.value.idc}-app"]
  db_subnet_ids = local.db_subnet_ids
  sg_sf_id      = module.network.security_group_ids["sf"]
  sg_core_id    = module.network.security_group_ids["core"]
}

module "global_zone" {
  source = "./modules/global-zone"

  prefix             = "${var.prefix}-global"
  zone               = local.idc_zones["idc1"]
  service_nodes      = var.resource_plan.global_zone.service_nodes
  mgmt_nodes         = var.resource_plan.global_zone.mgmt_nodes
  db_services        = ["gns", "rmb", "adm", "batch"] # one MySQL each, compose parity
  db_instance_class  = var.db_instance_class
  db_allocated_gb    = var.db_allocated_gb
  app_instance_type  = var.app_instance_type
  image_id           = var.image_id
  db_master_password = var.db_master_password

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
