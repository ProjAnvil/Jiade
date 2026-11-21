variable "prefix" {
  description = "DR site name prefix."
  type        = string
}

variable "vpc_cidr" {
  description = "DR site VPC CIDR (must not overlap the primary site)."
  type        = string
}

variable "zones" {
  description = "Two availability zones in the DR region."
  type        = list(string)
}

variable "db_instance_class" {
  description = "RDS instance class for the replicas."
  type        = string
}

variable "source_dbs" {
  description = "Map of DB name -> source ARN to replicate (units + global zone)."
  type        = map(string)
}

variable "resource_tags" {
  description = "Extra tags applied to all DR resources."
  type        = map(string)
  default     = {}
}
