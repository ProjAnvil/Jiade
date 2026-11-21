variable "prefix" {
  description = "DB layer name prefix."
  type        = string
}

variable "keeper_nodes" {
  description = "Number of DBKeeper nodes (odd count for raft majority; default 3)."
  type        = number
  default     = 3
}

variable "proxy_nodes" {
  description = "Number of DBProxy nodes (stateless L4; default 2)."
  type        = number
  default     = 2
}

variable "app_instance_type" {
  description = "EC2 instance type."
  type        = string
}

variable "image_id" {
  description = "EC2 AMI id."
  type        = string
}

variable "resource_tags" {
  description = "Extra tags applied to all DB layer resources."
  type        = map(string)
  default     = {}
}

variable "mgmt_subnet_id" {
  description = "MGMT subnet for the keepers (raft peers + API)."
  type        = string
}

variable "sf_subnet_id" {
  description = "SF subnet for the proxies (client side of the connection layer)."
  type        = string
}

variable "sg_dbkeeper_id" {
  type = string
}

variable "sg_dbproxy_id" {
  type = string
}
