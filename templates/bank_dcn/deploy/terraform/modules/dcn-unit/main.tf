# One DCN unit: app nodes plus a single unit database. The unit DB is
# Multi-AZ (primary + standby) when db_multi_az is set; AWS chooses the
# standby AZ itself, so the compose-style per-unit rotation is expressed on
# the app tier (each unit's app nodes live in the unit's own IDC).

resource "aws_instance" "app" {
  count = var.app_nodes

  ami                    = var.image_id
  instance_type          = var.app_instance_type
  subnet_id              = var.app_subnet_id
  vpc_security_group_ids = [var.sg_sf_id]

  root_block_device {
    volume_type = "gp3"
    volume_size = 8
  }

  tags = {
    Name = "${var.prefix}-app-${count.index + 1}"
  }

  # Key pair / user data are left to the deployment stage.
}

resource "aws_db_subnet_group" "unit" {
  name       = "${replace(var.prefix, "-", "_")}_db"
  subnet_ids = var.db_subnet_ids
}

resource "aws_db_instance" "unit" {
  identifier     = "${var.prefix}-db"
  engine         = "mysql"
  engine_version = "8.0"

  instance_class      = var.db_instance_class
  allocated_storage   = var.db_allocated_gb
  storage_type        = "gp3"
  multi_az            = var.db_multi_az
  publicly_accessible = false
  skip_final_snapshot = true
  username            = "dcnadmin"
  password            = var.db_master_password

  db_subnet_group_name   = aws_db_subnet_group.unit.name
  vpc_security_group_ids = [var.sg_core_id]

  tags = {
    Name = "${var.prefix}-db"
  }
}
