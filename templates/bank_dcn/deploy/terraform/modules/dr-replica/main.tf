# Remote disaster-recovery site (the second city): a small VPC in the DR
# region holding one asynchronous cross-region read replica per unit DB.
# Replication is async, so RPO > 0; a full failover (promote replicas, deploy
# app tier) is the deployment stage's job — this module provisions the data
# leg only.

resource "aws_vpc" "dr" {
  cidr_block = var.vpc_cidr

  tags = merge(var.resource_tags, { Name = "${var.prefix}-vpc" })
}

resource "aws_subnet" "db" {
  for_each = { for i, z in var.zones : "az${i + 1}" => z }

  vpc_id            = aws_vpc.dr.id
  availability_zone = each.value
  cidr_block        = cidrsubnet(var.vpc_cidr, 8, 2 + index(var.zones, each.value))

  tags = merge(var.resource_tags, { Name = "${var.prefix}-db-${each.key}" })
}

resource "aws_db_subnet_group" "dr" {
  name       = "${replace(var.prefix, "-", "_")}_db"
  subnet_ids = [for s in aws_subnet.db : s.id]

  tags = var.resource_tags
}

resource "aws_security_group" "db" {
  name        = "${var.prefix}-sg-db"
  description = "DR replicas: MySQL from the DR VPC only"
  vpc_id      = aws_vpc.dr.id

  tags = merge(var.resource_tags, { Name = "${var.prefix}-sg-db" })
}

resource "aws_vpc_security_group_ingress_rule" "db_vpc" {
  security_group_id = aws_security_group.db.id
  cidr_ipv4         = var.vpc_cidr
  ip_protocol       = "tcp"
  from_port         = 3306
  to_port           = 3306
}

resource "aws_vpc_security_group_egress_rule" "db_all" {
  security_group_id = aws_security_group.db.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_db_instance" "replica" {
  for_each = var.unit_dbs

  identifier          = "${var.prefix}-${each.key}-db"
  replicate_source_db = each.value
  instance_class      = var.db_instance_class
  publicly_accessible = false
  skip_final_snapshot = true

  db_subnet_group_name   = aws_db_subnet_group.dr.name
  vpc_security_group_ids = [aws_security_group.db.id]

  tags = merge(var.resource_tags, { Name = "${var.prefix}-${each.key}-db" })
}
