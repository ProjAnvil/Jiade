variable "prefix" {
  description = "Resource name prefix."
  type        = string
}

variable "vpc_cidr" {
  description = "VPC CIDR; per-role subnets are carved from it as /24s."
  type        = string
}

variable "idc_zones" {
  description = "Map of idc1/idc2 to same-city availability zones."
  type        = map(string)
}

variable "admin_cidr" {
  description = "CIDR allowed for SSH and management console ingress."
  type        = string
}
