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

variable "db_master_password" {
  description = "RDS master password."
  type        = string
  sensitive   = true
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
  description = "ServerFarm security group id for app nodes."
  type        = string
}

variable "sg_core_id" {
  description = "Core security group id for the unit DB."
  type        = string
}
