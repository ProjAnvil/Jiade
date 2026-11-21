# Global zone, split by security zone: SF holds the shared services (GNS
# routing, RMB coordination, ADM aggregation, batch scheduling) plus the
# self-hosted RabbitMQ + Redis middleware (ElastiCache / Amazon MQ are
# drop-ins at production scale); CORE holds one MySQL per service DB (placed
# on the per-IDC db subnets, guarded by the core SG); MGMT optionally holds
# observability/ops nodes; the DMZ holds an internal ALB standing in for the
# compose traefik unified entrypoint.

# ============ SF ============

resource "aws_instance" "svc" {
  count = var.service_nodes

  ami                    = var.image_id
  instance_type          = var.app_instance_type
  subnet_id              = var.sf_subnet_id
  vpc_security_group_ids = [var.sg_sf_id]

  root_block_device {
    volume_type = "gp3"
    volume_size = 8
  }

  tags = {
    Name = "${var.prefix}-svc-${count.index + 1}"
  }
}

# RabbitMQ and Redis live together on one node in the simulation; split them
# (or move to managed services) at production scale.
resource "aws_instance" "mq" {
  ami                    = var.image_id
  instance_type          = var.app_instance_type
  subnet_id              = var.sf_subnet_id
  vpc_security_group_ids = [var.sg_sf_id]

  root_block_device {
    volume_type = "gp3"
    volume_size = 8
  }

  tags = {
    Name = "${var.prefix}-mq-1"
  }
}

# ============ CORE ============

# Single-AZ matches the simulation; set multi_az on the module call for prod.
resource "aws_db_subnet_group" "svc" {
  name       = "${replace(var.prefix, "-", "_")}_db"
  subnet_ids = var.db_subnet_ids
}

resource "aws_db_instance" "svc_db" {
  for_each = toset(var.db_services)

  identifier     = "${var.prefix}-${each.key}-db"
  engine         = "mysql"
  engine_version = "8.0"

  instance_class      = var.db_instance_class
  allocated_storage   = var.db_allocated_gb
  storage_type        = "gp3"
  publicly_accessible = false
  skip_final_snapshot = true
  username            = "dcnadmin"
  password            = var.db_master_password

  db_subnet_group_name   = aws_db_subnet_group.svc.name
  vpc_security_group_ids = [var.sg_core_id]

  tags = {
    Name = "${var.prefix}-${each.key}-db"
  }
}

# ============ MGMT ============

resource "aws_instance" "mgmt" {
  count = var.mgmt_nodes

  ami                    = var.image_id
  instance_type          = var.app_instance_type
  subnet_id              = var.mgmt_subnet_id
  vpc_security_group_ids = [var.sg_mgmt_id]

  root_block_device {
    volume_type = "gp3"
    volume_size = 8
  }

  tags = {
    Name = "${var.prefix}-mgmt-${count.index + 1}"
  }
}

# ============ DMZ ============

# Unified ingress standing in for the compose traefik entrypoint (:18070).
# Internal on purpose: no IGW/NAT/EIP in this minimal VPC. Make it
# internet-facing (plus IGW and route tables) when actually exposing traffic.
# Listener + target binding is left to the deployment stage.
resource "aws_lb" "ingress" {
  name               = "${var.prefix}-ingress"
  internal           = true
  load_balancer_type = "application"
  subnets            = var.dmz_subnet_ids
  security_groups    = [var.sg_dmz_id]
}
