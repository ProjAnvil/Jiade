# Planning data. The whole topology (two same-city AZs, base DCN units,
# optional expansion unit, global zone sizing) is expressed here as data; the
# root module only maps the plan onto modules. Adding a unit to
# resource_plan.dcns provisions it, mirroring the compose expansion profile.

variable "resource_plan" {
  description = "Deployment plan: region, zones, CIDR, DCN units, expansion unit, global zone."
  type = object({
    region = string
    zones  = list(string)
    cidr   = string

    dcns = list(object({
      name      = string
      idc       = number # 1 or 2; unit app tier and DB live in the unit's AZ
      app_nodes = number
    }))

    expansion = object({
      enabled   = bool
      name      = string
      idc       = number
      app_nodes = number
    })

    global_zone = object({
      service_nodes = number # SF VMs (gns/rmb/adm/batch/console)
      mgmt_nodes    = number # MGMT VMs (observability/ops); 0 = subnet only
    })
  })

  validation {
    condition     = length(var.resource_plan.zones) == 2
    error_message = "Exactly two same-city availability zones are required (idc1/idc2)."
  }

  validation {
    condition     = alltrue([for d in var.resource_plan.dcns : contains([1, 2], d.idc)])
    error_message = "Each DCN unit must reference idc 1 or 2."
  }
}

variable "prefix" {
  description = "Resource name prefix."
  type        = string
  default     = "dcn"
}

# Minimal specs: this template simulates the DCN architecture; it is not
# sized for real traffic. Scale up only if actually deploying.

variable "app_instance_type" {
  description = "EC2 instance type for dcn-app and global-zone nodes (2 vCPU / 1 GiB)."
  type        = string
  default     = "t3.micro"
}

variable "image_id" {
  description = "EC2 AMI id matching app_instance_type architecture (x86_64 for t3)."
  type        = string
}

variable "db_instance_class" {
  description = "RDS instance class for unit and global-zone MySQL (2 vCPU / 1 GiB)."
  type        = string
  default     = "db.t3.micro"
}

variable "db_allocated_gb" {
  description = "RDS allocated storage in GB per instance."
  type        = number
  default     = 20
}

variable "db_multi_az" {
  description = "Run unit DBs Multi-AZ (primary + standby). AWS chooses the standby AZ itself."
  type        = bool
  default     = true
}

variable "db_master_password" {
  description = "RDS master password for every MySQL instance (8+ printable chars, no / @ \" space)."
  type        = string
  sensitive   = true
}

variable "admin_cidr" {
  description = "CIDR allowed to reach management ports. Tighten before real use."
  type        = string
  default     = "0.0.0.0/0"
}
