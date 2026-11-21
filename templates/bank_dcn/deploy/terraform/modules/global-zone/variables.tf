variable "prefix" {
  description = "Global zone name prefix, e.g. dcn-global."
  type        = string
}

variable "zone" {
  description = "Availability zone for shared-zone compute (SF/MGMT subnets)."
  type        = string
}

variable "service_nodes" {
  description = "Number of SF service EC2s (gns/rmb/adm/batch/console/monitoring)."
  type        = number
}

variable "mgmt_nodes" {
  description = "Number of MGMT EC2s (observability/ops). 0 keeps the zone subnet-only."
  type        = number
}

variable "db_services" {
  description = "Global-zone service names; one MySQL instance per entry."
  type        = list(string)
}

variable "db_instance_class" {
  description = "RDS instance class."
  type        = string
}

variable "db_allocated_gb" {
  description = "RDS allocated storage in GB."
  type        = number
}

variable "app_instance_type" {
  description = "EC2 instance type."
  type        = string
}

variable "image_id" {
  description = "EC2 AMI id."
  type        = string
}

variable "db_backup_retention_days" {
  description = "Automated backup retention in days (0 disables; set 7+ for production)."
  type        = number
}

variable "resource_tags" {
  description = "Extra tags applied to all global-zone resources."
  type        = map(string)
  default     = {}
}

variable "db_master_password" {
  description = "RDS master password."
  type        = string
  sensitive   = true
}

variable "vpc_id" {
  type = string
}

variable "sf_subnet_id" {
  description = "ServerFarm subnet id for shared services and middleware."
  type        = string
}

variable "mgmt_subnet_id" {
  description = "MGMT subnet id for observability nodes."
  type        = string
}

variable "dmz_subnet_ids" {
  description = "DMZ subnet pair (one per AZ) for the ingress load balancer."
  type        = list(string)
}

variable "db_subnet_ids" {
  description = "CORE db subnets across both IDCs (RDS subnet groups need two AZs)."
  type        = list(string)
}

variable "sg_dmz_id" {
  type = string
}

variable "sg_sf_id" {
  type = string
}

variable "sg_core_id" {
  type = string
}

variable "sg_mgmt_id" {
  type = string
}
