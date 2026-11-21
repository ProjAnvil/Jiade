# One VPC models the city. Subnets follow two orthogonal dimensions,
# mirroring the compose topology: security zones (dmz / sf / core / mgmt,
# WeBank-style) and IDC fault domains (idc1 / idc2). Zones govern security;
# IDCs govern blast radius. CORE placement for unit DBs is the per-IDC db
# subnets guarded by the core security group.

locals {
  # Role -> /24 offset within an IDC's 16-subnet block.
  subnet_offsets = {
    app = 1
    db  = 2
  }

  idc_subnets = merge([
    for idc, zone in var.idc_zones : {
      for role, offset in local.subnet_offsets :
      "${idc}-${role}" => {
        zone = zone
        # idc1 gets /24s at offsets 1-2, idc2 at 17-18.
        cidr = cidrsubnet(var.vpc_cidr, 8, (tonumber(trimprefix(idc, "idc")) - 1) * 16 + offset)
      }
    }
  ]...)

  # Shared zones: ServerFarm and MGMT live in idc1's zone; the DMZ pair spans
  # both zones because the ALB needs one subnet per AZ.
  global_subnets = {
    sf   = { zone = var.idc_zones["idc1"], cidr = cidrsubnet(var.vpc_cidr, 8, 100) }
    mgmt = { zone = var.idc_zones["idc1"], cidr = cidrsubnet(var.vpc_cidr, 8, 101) }
  }

  subnet_map = merge(
    local.idc_subnets,
    { "dmz-a" = { zone = var.idc_zones["idc1"], cidr = cidrsubnet(var.vpc_cidr, 8, 3) } },
    { "dmz-b" = { zone = var.idc_zones["idc2"], cidr = cidrsubnet(var.vpc_cidr, 8, 19) } },
    local.global_subnets,
  )
}

resource "aws_vpc" "main" {
  cidr_block = var.vpc_cidr

  tags = {
    Name = "${var.prefix}-vpc"
  }
}

resource "aws_subnet" "main" {
  for_each = local.subnet_map

  vpc_id            = aws_vpc.main.id
  availability_zone = each.value.zone
  cidr_block        = each.value.cidr

  tags = {
    Name = "${var.prefix}-${each.key}"
  }
}

# ============ Security groups per zone ============

resource "aws_security_group" "dmz" {
  name        = "${var.prefix}-sg-dmz"
  description = "DMZ ingress proxies (ALB); no business logic"
  vpc_id      = aws_vpc.main.id
}

resource "aws_security_group" "sf" {
  name        = "${var.prefix}-sg-sf"
  description = "ServerFarm app and shared-service nodes"
  vpc_id      = aws_vpc.main.id
}

resource "aws_security_group" "core" {
  name        = "${var.prefix}-sg-core"
  description = "Core zone MySQL instances (unit DBs included)"
  vpc_id      = aws_vpc.main.id
}

resource "aws_security_group" "mgmt" {
  name        = "${var.prefix}-sg-mgmt"
  description = "Management and observability nodes"
  vpc_id      = aws_vpc.main.id
}

# DMZ: internal ALB accepts traffic from inside the VPC only (make it
# internet-facing plus IGW when actually exposing traffic).
resource "aws_vpc_security_group_ingress_rule" "dmz_vpc" {
  security_group_id = aws_security_group.dmz.id
  cidr_ipv4         = var.vpc_cidr
  ip_protocol       = "tcp"
  from_port         = 80
  to_port           = 80
}

locals {
  # SSH plus service and management consoles (rabbitmq :15672, prometheus
  # :9090, grafana :3000, traefik metrics :8082, console :8080). Tighten
  # admin_cidr for real use.
  admin_ports = [22, 3000, 8080, 8082, 9090, 15672]
}

# SF: service HTTP (8080, from DMZ targets / SF peers / MGMT scraping) and
# AMQP (5672, SF members) inside the VPC; admin consoles from admin_cidr.
resource "aws_vpc_security_group_ingress_rule" "sf_vpc_http" {
  security_group_id = aws_security_group.sf.id
  cidr_ipv4         = var.vpc_cidr
  ip_protocol       = "tcp"
  from_port         = 8080
  to_port           = 8080
}

resource "aws_vpc_security_group_ingress_rule" "sf_vpc_amqp" {
  security_group_id = aws_security_group.sf.id
  cidr_ipv4         = var.vpc_cidr
  ip_protocol       = "tcp"
  from_port         = 5672
  to_port           = 5672
}

resource "aws_vpc_security_group_ingress_rule" "sf_admin" {
  for_each = { for p in local.admin_ports : tostring(p) => p }

  security_group_id = aws_security_group.sf.id
  cidr_ipv4         = var.admin_cidr
  ip_protocol       = "tcp"
  from_port         = each.value
  to_port           = each.value
}

# CORE: MySQL only from SF services and from MGMT exporters.
resource "aws_vpc_security_group_ingress_rule" "core_from_sf" {
  security_group_id            = aws_security_group.core.id
  referenced_security_group_id = aws_security_group.sf.id
  ip_protocol                  = "tcp"
  from_port                    = 3306
  to_port                      = 3306
}

resource "aws_vpc_security_group_ingress_rule" "core_from_mgmt" {
  security_group_id            = aws_security_group.core.id
  referenced_security_group_id = aws_security_group.mgmt.id
  ip_protocol                  = "tcp"
  from_port                    = 3306
  to_port                      = 3306
}

# MGMT: observability consoles from admin_cidr only.
resource "aws_vpc_security_group_ingress_rule" "mgmt_admin" {
  for_each = { for p in local.admin_ports : tostring(p) => p }

  security_group_id = aws_security_group.mgmt.id
  cidr_ipv4         = var.admin_cidr
  ip_protocol       = "tcp"
  from_port         = each.value
  to_port           = each.value
}

resource "aws_vpc_security_group_egress_rule" "all" {
  for_each = {
    dmz  = aws_security_group.dmz.id
    sf   = aws_security_group.sf.id
    core = aws_security_group.core.id
    mgmt = aws_security_group.mgmt.id
  }

  security_group_id = each.value
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}
