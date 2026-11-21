variable "prefix" {
  description = "Unit name prefix, e.g. dcn-dcn01."
  type        = string
}

variable "primary_zone" {
  description = "Availability zone holding the unit app tier."
  type        = string
}

variable "app_nodes" {
  description = "Number of dcn-app EC2 nodes."
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

variable "db_instance_class" {
  description = "RDS instance class."
  type        = string
}

variable "db_allocated_gb" {
  description = "RDS allocated storage in GB."
  type        = number
}

variable "db_multi_az" {
  description = "Run the unit DB Multi-AZ (primary + standby)."
  type        = bool
}

variable "db_read_replica" {
  description = "Add one in-region read replica (one primary, one standby, one read replica)."
  type        = bool
}

variable "db_backup_retention_days" {
  description = "Automated backup retention in days (0 disables; set 7+ for production)."
  type        = number
}

variable "db_master_password" {
  description = "RDS master password."
  type        = string
  sensitive   = true
}

variable "resource_tags" {
  description = "Extra tags applied to all unit resources."
  type        = map(string)
  default     = {}
}

variable "vpc_id" {
  type = string
}

variable "app_subnet_id" {
  description = "App subnet in the unit's IDC."
  type        = string
}

variable "db_subnet_ids" {
  description = "DB subnets across both IDCs (RDS subnet groups need two AZs)."
  type        = list(string)
}

variable "sg_sf_id" {
  description = "Shared-zone (ServerFarm) security group id; allowed to reach this unit."
  type        = string
}

variable "sg_core_id" {
  description = "Core security group id for the unit DB."
  type        = string
}

variable "sg_dbproxy_id" {
  description = "DB connection-layer security group id; this unit's apps route MySQL through it."
  type        = string
}

variable "sg_mgmt_id" {
  description = "MGMT security group id; allowed to scrape this unit."
  type        = string
}
