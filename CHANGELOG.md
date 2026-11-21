# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.5.0] - 2026-11-21

### Added

- **bank_dcn: AWS Terraform layer** (`deploy/terraform`) with two ready-made plans:
  Plan A (`terraform.tfvars.example`) — same-city active-active across two AZs;
  Plan B (`terraform.dr.tfvars.example`) — Plan A plus a second-city region with
  one asynchronous cross-region read replica per database (unit DBs and the
  global-zone gns/rmb/adm/batch DBs). Plans are data-driven by a single
  `resource_plan` variable; adding a DCN unit is a purely additive apply.
- **bank_dcn: DB middleware layer** (self-managed control plane, the open-source
  counterpart of a TDSQL-style gateway):
  - `db-keeper` x3 — raft quorum (`hashicorp/raft`) for arbitration; probes
    backends, bootstraps GTID replication, auto-fails over after three failed
    primary probes with a caught-up candidate; the switch procedure is
    fence-then-promote (super_read_only -> promote -> re-attach the old primary
    -> repoint routing). On managed backends (RDS) sets run in probe-and-route
    mode and failover belongs to the managed service.
  - `db-proxy` x2 — stateless L4 connection layer (port-per-set); routing is
    pushed by the keeper, connections snapshot their backend, and
    `/admin/drain` acts as the network-side fence.
  - `db-admin` — Vue 3 + DaisyUI console: topology, health/replication lag,
    manual switch with confirmation, event timeline.
- **bank_dcn: WeBank-style security zones** in the compose topology:
  `dmz` / `sf` / `core` / `mgmt` zones crossed with `idc1` / `idc2` app fault
  domains and `idc1-db` / `idc2-db` DB networks. Applications never join a DB
  network — every MySQL connection goes through the proxy layer.
- **bank_dcn: deployment stage** codified as `deploy/terraform/scripts/deploy-aws.sh`
  (image build/push to ECR, SSM bootstrap, internet egress, SQL init with
  topology-derived GNS routes, traefik gateway, per-unit containers, MGMT
  observability stack, ALB listener) plus `verify-aws.sh`, a cloud smoke check.
- bank_dcn: verify gate 9 — DB-layer primary switch round-trip with exact
  balance conservation through the promoted replica; topology contract
  extended to 24 assertions.

### Changed

- bank_dcn: all application DSNs now resolve through `db-proxy`
  (units 23301-23304, global services 23310-23313) instead of direct
  database hosts.
- bank_dcn: `dcn01` runs as a self-managed replication demo set (primary +
  GTID replica) orchestrated by the keeper; other units remain single
  instances simulating the managed form.
- bank_dcn: template READMEs, `ARCHITECTURE.md`, and port tables updated for
  the zone model and the middleware layer; design credits reference WeBank's
  public DCN practice and `WeBankPartners/delivery-by-terraform` (Apache-2.0).

### Security

- bank_dcn (Terraform): network-enforced DB isolation — the core security
  group accepts 3306 only from the proxy/keeper/mgmt groups; per-unit app
  security groups are granted only the proxy port range (23300-23399).
- bank_dcn (Terraform): at-rest encryption for RDS storage and EBS root
  volumes, automated-backup retention knob (auto-raised to >= 1 day when DR
  replicas are enabled), optional in-region read replicas, resource tags,
  and a commented S3 remote-state backend template.

## [0.4.2] - 2026-11-13

### Changed

- bank & commerce templates: split into flat independent Go projects with
  per-project unit coverage; unified compose conventions; English-only code
  and commits.

## [0.4.1] - 2026-11-12

### Changed

- bank_dcn template: split into flat independent Go projects with per-project
  unit coverage.

## [0.4.0] - 2026-08-02

### Added

- New template: `bank_dcn` — DCN unitized architecture simulation
  (GNS routing, RMB reliable message bus, self-contained DCN units, ADM
  aggregation, verify gates).

## [0.3.1] - 2026-07-31

### Added

- Bank template: durable payment saga and operational closure.

## [0.3.0] - 2026-07-24

### Added

- Commerce template: a second industry microcosm.

## [0.2.0] - 2026-07-23

### Changed

- HTTP service boundaries; removed foreign-data-wrapper coupling.

## [0.1.1] - 2026-07-20

### Changed

- CLI defaults, port relocation, English-only artifacts.

## [0.1.0] - 2026-07-17

### Added

- Initial release: `jiade` — runnable industry-system microcosms.

[Unreleased]: https://github.com/ProjAnvil/Jiade/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/ProjAnvil/Jiade/compare/v0.4.2...v0.5.0
[0.4.2]: https://github.com/ProjAnvil/Jiade/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/ProjAnvil/Jiade/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/ProjAnvil/Jiade/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/ProjAnvil/Jiade/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/ProjAnvil/Jiade/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ProjAnvil/Jiade/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/ProjAnvil/Jiade/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/ProjAnvil/Jiade/releases/tag/v0.1.0
