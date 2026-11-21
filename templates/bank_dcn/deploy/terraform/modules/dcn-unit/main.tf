# One DCN unit: app nodes plus a single unit database. The unit DB is
# Multi-AZ (primary + standby) when db_multi_az is set, with an optional
# in-region read replica; AWS chooses the standby AZ itself, so the
# compose-style per-unit rotation is expressed on the app tier (each unit's
# app nodes live in the unit's own IDC).
#
# Unit isolation: each unit gets its OWN app security group. It accepts :8080
# only from the shared zone (traefik/gns/rmb/adm/batch) and from MGMT —
# never from other units, enforcing the "no direct unit-to-unit traffic,
# cross-unit goes through RMB" rule at the network layer.

resource "aws_security_group" "unit" {
  name        = "${var.prefix}-sg-unit"
  description = "Per-unit app isolation: 8080 from SF/MGMT only, no unit-to-unit"
  vpc_id      = var.vpc_id

  tags = merge(var.resource_tags, { Name = "${var.prefix}-sg-unit" })
}

resource "aws_vpc_security_group_ingress_rule" "unit_from_sf" {
  security_group_id            = aws_security_group.unit.id
  referenced_security_group_id = var.sg_sf_id
  ip_protocol                  = "tcp"
  from_port                    = 8080
  to_port                      = 8080
}

resource "aws_vpc_security_group_ingress_rule" "unit_from_mgmt" {
  security_group_id            = aws_security_group.unit.id
  referenced_security_group_id = var.sg_mgmt_id
  ip_protocol                  = "tcp"
  from_port                    = 8080
  to_port                      = 8080
}

# Grant this unit's app security group access to the DB connection layer
# (port-per-set range). Units reach MySQL ONLY through this path.
resource "aws_vpc_security_group_ingress_rule" "dbproxy_from_unit" {
  security_group_id            = var.sg_dbproxy_id
  referenced_security_group_id = aws_security_group.unit.id
  ip_protocol                  = "tcp"
  from_port                    = 23300
  to_port                      = 23399
}

resource "aws_vpc_security_group_egress_rule" "unit_all" {
  security_group_id = aws_security_group.unit.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_instance" "app" {
  count = var.app_nodes

  ami                    = var.image_id
  instance_type          = var.app_instance_type
  subnet_id              = var.app_subnet_id
  vpc_security_group_ids = [aws_security_group.unit.id]

  root_block_device {
    volume_type = "gp3"
    volume_size = 8
    encrypted   = true
  }

  tags = merge(var.resource_tags, { Name = "${var.prefix}-app-${count.index + 1}" })

  # Key pair / user data are left to the deployment stage.
}

resource "aws_db_subnet_group" "unit" {
  name       = "${replace(var.prefix, "-", "_")}_db"
  subnet_ids = var.db_subnet_ids

  tags = var.resource_tags
}

resource "aws_db_instance" "unit" {
  identifier     = "${var.prefix}-db"
  engine         = "mysql"
  engine_version = "8.0"

  instance_class          = var.db_instance_class
  allocated_storage       = var.db_allocated_gb
  storage_type            = "gp3"
  storage_encrypted       = true
  multi_az                = var.db_multi_az
  backup_retention_period = var.db_backup_retention_days
  publicly_accessible     = false
  skip_final_snapshot     = true
  username                = "dcnadmin"
  password                = var.db_master_password

  db_subnet_group_name   = aws_db_subnet_group.unit.name
  vpc_security_group_ids = [var.sg_core_id]

  tags = merge(var.resource_tags, { Name = "${var.prefix}-db" })
}

# Optional in-region read replica: gives the one-primary-one-standby-one-
# replica topology WeBank uses (batch/ADM reads go to the replica).
resource "aws_db_instance" "unit_replica" {
  count = var.db_read_replica ? 1 : 0

  identifier          = "${var.prefix}-db-ro"
  replicate_source_db = aws_db_instance.unit.arn
  instance_class      = var.db_instance_class
  publicly_accessible = false
  skip_final_snapshot = true

  vpc_security_group_ids = [var.sg_core_id]

  tags = merge(var.resource_tags, { Name = "${var.prefix}-db-ro" })
}
