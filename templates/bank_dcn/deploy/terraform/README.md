# Terraform provisioning layer (AWS)

Provisions the bank_dcn simulation topology on AWS: two same-city
availability zones standing in for the compose `idc1`/`idc2` networks, one
module call per DCN unit with the app tier alternating across IDCs, and a
global zone for GNS / RMB / ADM / batch / monitoring. This directory covers
the provisioning stage only — building images and rolling out containers
(deployment stage) is out of scope here.

Specs are deliberately **simulation-minimal** (`t3.micro` EC2,
`db.t3.micro` RDS, 8–20 GB gp3): this template exists to model the DCN
topology, not to carry real traffic. Scale the variables up only for an
actual deployment.

## Layout

```text
terraform/
├── main.tf                 # maps resource_plan onto modules (planning data -> resources)
├── variables.tf            # resource_plan: zones, DCN units, expansion unit, global zone
├── topology.tf             # renders generated/dcn-topology.json for the deployment stage
├── terraform.tfvars.example        # base plan: dcn01/02/03 + expansion dcn04 (disabled)
├── terraform.increment.tfvars.example # base + dcn04 appended (the increment demo)
└── modules/
    ├── network/            # VPC, zone subnets (dmz-a/b, sf, mgmt) + per-IDC app/db subnets, zone SGs
    ├── dcn-unit/           # per-unit app EC2s (SF) + Multi-AZ RDS MySQL (CORE, primary + standby)
    └── global-zone/        # SF service/mq EC2s, CORE per-service RDS, MGMT nodes, DMZ internal ALB
```

## Compose-to-cloud mapping

| compose | terraform (AWS) |
|---|---|
| networks `dmz` / `sf` / `core` / `mgmt` / `idc1` / `idc2` | one VPC: dmz-a/dmz-b + sf + mgmt subnets, per-IDC app (SF) and db (CORE) subnets, zone SGs (dmz/sf/core/mgmt) |
| `dcnXX-db` (single MySQL 8) | `aws_db_instance` MySQL 8, Multi-AZ (primary + standby) |
| `dcnXX-app` | `aws_instance` x `app_nodes` |
| `gns` / `rmb-coordinator` / `adm` / `batch-scheduler` / `console` / monitoring | `global-zone` service EC2s |
| `gns-redis` + `dcn-rabbitmq` | self-hosted on the `mq` EC2 (ElastiCache / Amazon MQ are drop-ins) |
| `gns-db` / `rmb-db` / `adm-db` / `batch-db` | one single-AZ `aws_db_instance` each |
| `traefik :18070` | internal `aws_lb` (listener wiring = deployment stage) |
| `--profile expansion` (dcn04) | `resource_plan.expansion.enabled` flag |

## Usage

```bash
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
cp terraform.tfvars.example terraform.tfvars  # then edit image_id / db_master_password / admin_cidr
terraform init
terraform plan
terraform apply
```

The provider block sets `skip_credentials_validation` /
`skip_requesting_account_id` / `skip_metadata_api_check`, so `terraform plan`
runs fully offline against a var file (the whole template is inspectable
without an AWS account). Remove those flags for a real deployment.

After the deployment stage, run the cloud smoke check from inside the VPC
(e.g. on the SF node over SSM; idempotent, safe to rerun):

```bash
GATEWAY=http://internal-<alb-dns> PROM_URL=http://<mgmt-ip>:9090 ./verify-aws.sh
```

It covers the cloud counterpart of compose verify gates 1/2/7: route table,
gateway healthz, intra/cross-DCN transfers (auto-skipped on rerun), ADM
reconciliation, and Prometheus target health.

Adding a unit = appending one entry to `resource_plan.dcns` (or flipping
`expansion.enabled`) and applying again — the same online-expansion story as
the compose `expansion` profile, at the infrastructure layer. A one-unit base
works the same way: set `dcns` to a single entry.

## Primary/standby, incremental growth, and unit awareness

**Primary/standby.** Unit DBs run Multi-AZ (`db_multi_az`, default on): one
primary plus an AWS-managed standby in another AZ, failover included. AWS
chooses the standby AZ itself, so the compose-style per-unit rotation is
expressed on the app tier — each unit's app nodes sit in the unit's own IDC
(dcn01→idc1, dcn02→idc2, ...), and a single-IDC failure never removes all
units' app tiers. Global-zone service DBs are single-AZ (simulation parity).

**Increment.** Compare the two example var files:

```bash
terraform plan -var-file=terraform.tfvars.example -var db_master_password=... -out=base.plan
terraform plan -var-file=terraform.increment.tfvars.example -var db_master_password=... -out=inc.plan
```

Verified offline: the increment plan contains exactly the base plan plus
`module.dcn_unit["dcn04"]` (app EC2 + RDS instance) — nothing else changes.
That is the Terraform-idiomatic increment: one state, one plan list, additive
diffs. Separate per-DCN TF files are deliberately avoided (split state,
cross-references only via remote state — worse, not better).

**Unit awareness — what TF can and cannot do.** Terraform provisions
infrastructure; it cannot make running services perceive each other. What it
does provide is static, plan-driven awareness: every module reads the same
`resource_plan`, and `topology.tf` renders `generated/dcn-topology.json`
(units, zones, DB endpoints, app IPs, global-zone endpoints) as the handoff
artifact for the deployment stage. Runtime awareness stays where the compose
simulation puts it: GNS `POST /routes` registers the new unit's segment,
existing units pick it up through GNS, and traffic switches — that is the
compose expansion gate, replayed after `terraform apply` by the deployment
stage, not by Terraform itself.

## Roadmap

- Ingress ALB listener + target-group binding (deployment stage).
- Deployment stage proper: build service images and roll them out (ansible /
  kubectl / helm) after provisioning.
- Managed Redis / RabbitMQ (ElastiCache / Amazon MQ) instead of self-hosted.
- Remote-dr second region (`dr` site in the plan) and cross-region replication.
- Cross-DCN isolation: dedicated security groups per unit (currently one app
  SG for all units, compose parity).

## Design credits

The DCN (Data Center Node) unitized architecture follows WeBank's public
practice; the planning-data-driven Terraform layering is inspired by
[WeBankPartners/delivery-by-terraform](https://github.com/WeBankPartners/delivery-by-terraform)
(Apache-2.0). All code here is an independent implementation. See the template
README's Design Credits section for links.
