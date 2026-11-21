# DB middleware layer (the self-managed control plane the compose stack
# runs): DBKeeper x N forms the raft arbitration quorum on the MGMT subnet
# and reaches the CORE/idc-db subnets for probes and switch execution;
# DBProxy x N sits on SF and bridges applications (port-per-set 23300+) to
# MySQL. Applications themselves have no route to port 3306 — enforced by
# the core/dbproxy security groups (see modules/network).

resource "aws_instance" "keeper" {
  count = var.keeper_nodes

  ami                    = var.image_id
  instance_type          = var.app_instance_type
  subnet_id              = var.mgmt_subnet_id
  vpc_security_group_ids = [var.sg_dbkeeper_id]

  root_block_device {
    volume_type = "gp3"
    volume_size = 8
    encrypted   = true
  }

  tags = merge(var.resource_tags, { Name = "${var.prefix}-keeper-${count.index + 1}" })

  # The keeper image and SETS registration are deployment-stage concerns.
}

resource "aws_instance" "proxy" {
  count = var.proxy_nodes

  ami                    = var.image_id
  instance_type          = var.app_instance_type
  subnet_id              = var.sf_subnet_id
  vpc_security_group_ids = [var.sg_dbproxy_id]

  root_block_device {
    volume_type = "gp3"
    volume_size = 8
    encrypted   = true
  }

  tags = merge(var.resource_tags, { Name = "${var.prefix}-proxy-${count.index + 1}" })
}
