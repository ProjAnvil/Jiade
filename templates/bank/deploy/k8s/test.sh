#!/usr/bin/env bash
# Render the bank Kubernetes manifests and assert their shape.
#
# Renders the base, the dev overlay, and the prod overlay; each render is
# checked for the properties required by the operational-closure plan.
#
# The host has no `yq` binary; assertions are implemented with python3 + PyYAML.
# Run from anywhere — paths resolve relative to this script.
#
#   bash templates/bank/deploy/k8s/test.sh
#
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bank_root="$(cd "${script_dir}/../.." && pwd)"
base_dir="${bank_root}/deploy/k8s/base"
dev_dir="${bank_root}/deploy/k8s/overlays/dev"
prod_dir="${bank_root}/deploy/k8s/overlays/prod"

if [ ! -d "${base_dir}" ]; then
  echo "FAIL: base directory not found: ${base_dir}" >&2
  exit 2
fi

base_render="$(mktemp -t bank-k8s-base-XXXXXX.yaml)"
dev_render="$(mktemp -t bank-k8s-dev-XXXXXX.yaml)"
prod_render="$(mktemp -t bank-k8s-prod-XXXXXX.yaml)"
trap 'rm -f "${base_render}" "${dev_render}" "${prod_render}"' EXIT

echo "rendering ${base_dir}"
kubectl kustomize "${base_dir}" >"${base_render}"

echo "rendering ${dev_dir}"
kubectl kustomize "${dev_dir}" >"${dev_render}"

echo "rendering ${prod_dir}"
kubectl kustomize "${prod_dir}" >"${prod_render}"

python3 - "${base_render}" "${dev_render}" "${prod_render}" <<'PY'
import sys, yaml

base_path, dev_path, prod_path = sys.argv[1], sys.argv[2], sys.argv[3]

def load(path):
    with open(path) as f:
        return [d for d in yaml.safe_load_all(f) if d]

base_docs = load(base_path)
dev_docs  = load(dev_path)
prod_docs = load(prod_path)

def names_of(docs, kind):
    return [d["metadata"]["name"] for d in docs if d.get("kind") == kind]

errors = []
def check(cond, msg):
    if not cond:
        errors.append(msg)

SERVICES = ["core-banking", "customer", "payment", "reward", "risk", "loan", "wealth"]
PG_DBS = ["core-banking-db", "customer-db", "payment-db", "reward-db",
          "risk-db", "loan-db", "wealth-db"]
REPLICAS = {"core-banking": 2, "customer": 1, "payment": 2,
            "reward": 1, "risk": 2, "loan": 1, "wealth": 1}

# =====================================================================
# BASE: application shape (existing chassis assertions, refreshed for the
# payment-admin Service and the NetworkPolicy baseline).
# =====================================================================
deployments = [d for d in base_docs if d.get("kind") == "Deployment"]
hpas        = [d for d in base_docs if d.get("kind") == "HorizontalPodAutoscaler"]
pdbs        = [d for d in base_docs if d.get("kind") == "PodDisruptionBudget"]
services    = [d for d in base_docs if d.get("kind") == "Service"]
ingresses   = [d for d in base_docs if d.get("kind") == "Ingress"]
configmaps  = [d for d in base_docs if d.get("kind") == "ConfigMap"]
namespaces  = [d for d in base_docs if d.get("kind") == "Namespace"]
netpol_base = [d for d in base_docs if d.get("kind") == "NetworkPolicy"]

# --- Exact counts ---
check(len(deployments) == 7,
      f"[base] expected 7 Deployments, got {len(deployments)} ({names_of(base_docs,'Deployment')})")
check(len(hpas) == 7,
      f"[base] expected 7 HPAs, got {len(hpas)} ({names_of(base_docs,'HorizontalPodAutoscaler')})")
check(len(pdbs) == 3,
      f"[base] expected 3 PDBs, got {len(pdbs)} ({names_of(base_docs,'PodDisruptionBudget')})")

# --- Namespace present ---
check(any(ns["metadata"]["name"] == "bank" for ns in namespaces),
      "[base] expected a Namespace named 'bank'")

# --- Each Deployment: name, ports, replicas, command ---
deploy_by_name = {d["metadata"]["name"]: d for d in deployments}
check(set(deploy_by_name) == set(SERVICES),
      f"[base] Deployment names mismatch: {sorted(deploy_by_name)}")

for name in SERVICES:
    d = deploy_by_name.get(name)
    if not d:
        continue
    spec = d["spec"]["template"]["spec"]
    ctrs = spec.get("containers", [])
    check(len(ctrs) == 1, f"[base] {name}: expected exactly 1 container, got {len(ctrs)}")
    if not ctrs:
        continue
    c = ctrs[0]
    ports = {p.get("name"): p.get("containerPort") for p in c.get("ports", [])}
    check(ports.get("http") == 8080,
          f"[base] {name}: expected containerPort http=8080, got {ports}")
    check(ports.get("grpc") == 9090,
          f"[base] {name}: expected containerPort grpc=9090, got {ports}")
    check(d["spec"].get("replicas") == REPLICAS[name],
          f"[base] {name}: expected replicas={REPLICAS[name]}, got {d['spec'].get('replicas')}")
    img = c.get("image", "")
    check(img != "", f"[base] {name}: container image is empty")
    cmd = c.get("command")
    check(cmd == ["/usr/local/bin/service"],
          f"[base] {name}: expected command=['/usr/local/bin/service'], got {cmd}")

images = {d["spec"]["template"]["spec"]["containers"][0]["image"] for d in deployments}
check(len(images) == 1,
      f"[base] expected all Deployments to share one image, got {sorted(images)}")

# --- Service shapes: REST ClusterIP + headless <name>-grpc + payment-admin ---
svc_names = {s["metadata"]["name"] for s in services}
for name in SERVICES:
    rest = [s for s in services if s["metadata"]["name"] == name]
    check(len(rest) == 1,
          f"[base] {name}: expected one REST Service named '{name}', got {len(rest)}")
    if rest:
        rspec = rest[0]["spec"]
        check(rspec.get("clusterIP") != "None",
              f"[base] {name}: REST Service must not be headless")
        tports = {p.get("name"): p.get("port") for p in rspec.get("ports", [])}
        check(tports.get("http") == 8080,
              f"[base] {name}: REST Service must expose port http=8080, got {tports}")

    grpc_name = f"{name}-grpc"
    grpc = [s for s in services if s["metadata"]["name"] == grpc_name]
    check(len(grpc) == 1,
          f"[base] {name}: expected one headless Service '{grpc_name}', got {len(grpc)}")
    if grpc:
        gspec = grpc[0]["spec"]
        check(gspec.get("clusterIP") == "None",
              f"[base] {grpc_name}: expected clusterIP=None, got {gspec.get('clusterIP')}")
        check(gspec.get("publishNotReadyAddresses") is False,
              f"[base] {grpc_name}: expected publishNotReadyAddresses=false")
        tports = {p.get("name"): p.get("port") for p in gspec.get("ports", [])}
        check(tports.get("grpc") == 9090,
              f"[base] {grpc_name}: headless Service must expose grpc=9090, got {tports}")

# payment-admin: the protected operator admin gRPC Service (port 9091).
pa = [s for s in services if s["metadata"]["name"] == "payment-admin"]
check(len(pa) == 1,
      f"[base] expected one payment-admin Service, got {len(pa)}")
if pa:
    pa_spec = pa[0]["spec"]
    check(pa_spec.get("clusterIP") == "None",
          "[base] payment-admin: expected clusterIP=None")
    check(pa_spec.get("publishNotReadyAddresses") is False,
          "[base] payment-admin: expected publishNotReadyAddresses=false")
    tports = {p.get("name"): p.get("port") for p in pa_spec.get("ports", [])}
    check(tports.get("admin-grpc") == 9091,
          f"[base] payment-admin: expected admin-grpc=9091, got {tports}")

# 7 REST + 7 app-grpc + 1 payment-admin = 15.
check(len(services) == 15,
      f"[base] expected 15 Services (7 REST + 7 grpc + payment-admin), "
      f"got {len(services)}: {sorted(svc_names)}")

# --- Ingress: only public REST prefixes, no gRPC, no admin, no /internal/ ---
ingress_blob = yaml.safe_dump(ingresses)
check("/internal/" not in ingress_blob,
      "[base] Ingress must not route any /internal/ prefix")
check("9090" not in ingress_blob,
      "[base] Ingress must not reference the gRPC/9090 port")
check("9091" not in ingress_blob,
      "[base] Ingress must not reference the admin-gRPC/9091 port")
check(len(ingresses) >= 1,
      f"[base] expected at least one Ingress, got {len(ingresses)}")

for ing in ingresses:
    for rule in ing["spec"].get("rules", []):
        paths = rule.get("http", {}).get("paths", [])
        for p in paths:
            be = p.get("backend", {})
            svc_name = be.get("service", {}).get("name", "")
            sport = (be.get("service", {}).get("port", {}) or {})
            port_name = sport.get("name")
            port_num  = sport.get("number")
            check(svc_name in SERVICES,
                  f"[base] Ingress backend '{svc_name}' is not a REST Service")
            check(port_name == "http" or port_num == 8080,
                  f"[base] Ingress backend for '{svc_name}' targets "
                  f"{port_name or port_num}, expected http/8080")
            pp = p.get("path", "")
            check(pp.startswith("/api/v1/"),
                  f"[base] Ingress path '{pp}' is not a public /api/v1/* prefix")

# --- Availability ---
pdb_names = {p["metadata"]["name"] for p in pdbs}
check(pdb_names == {"core-banking", "payment", "risk"},
      f"[base] PDB names mismatch: {sorted(pdb_names)}")

hpa_names = {h["metadata"]["name"] for h in hpas}
check(hpa_names == set(SERVICES),
      f"[base] HPA names mismatch: {sorted(hpa_names)}")

for h in hpas:
    ref = h["spec"]["scaleTargetRef"]
    check(ref.get("kind") == "Deployment",
          f"[base] {h['metadata']['name']}: HPA must target a Deployment")
    metrics = h["spec"].get("metrics", [])
    check(all(m.get("type") == "Resource" for m in metrics),
          f"[base] {h['metadata']['name']}: HPA must use Resource metrics only")
    check(any(m.get("resource", {}).get("name") == "cpu" for m in metrics),
          f"[base] {h['metadata']['name']}: HPA must have a cpu resource metric")

for p in pdbs:
    ref = p["spec"]["selector"]
    check("matchLabels" in ref,
          f"[base] {p['metadata']['name']}: PDB must use a label selector")

# --- ConfigMap: non-secret defaults ---
cms = [c for c in configmaps if c["metadata"]["name"] == "bank-config"]
check(len(cms) == 1,
      f"[base] expected exactly one ConfigMap 'bank-config', got {len(cms)}")
if cms:
    cm_blob = yaml.safe_dump(cms[0])
    check("DB_PASSWORD" not in cm_blob,
          "[base] ConfigMap must NOT contain DB_PASSWORD (secret)")

# --- NetworkPolicy baseline (Task 5 Step 4) ---
np_names = {n["metadata"]["name"] for n in netpol_base}
# Default-deny baseline.
check("bank-default-deny-ingress" in np_names,
      "[base] missing bank-default-deny-ingress NetworkPolicy")
check("bank-default-deny-egress" in np_names,
      "[base] missing bank-default-deny-egress NetworkPolicy")
# Task 3's focused admin-gRPC lockdown must still be present.
check("payment-admin-ingress" in np_names,
      "[base] missing payment-admin-ingress NetworkPolicy (Task 3)")
# Explicit allow rules for the documented flows.
for required in ["allow-dns-egress", "allow-gateway-to-rest",
                 "allow-internal-grpc-ingress", "allow-app-grpc-egress",
                 "allow-app-data-egress", "allow-otlp-egress",
                 "allow-prometheus-scrape"]:
    check(required in np_names,
          f"[base] missing NetworkPolicy '{required}' from the allow matrix")

# The payment-admin policy must restrict ingress to port 9091 only.
pa_np = [n for n in netpol_base if n["metadata"]["name"] == "payment-admin-ingress"]
if pa_np:
    ports = set()
    for rule in pa_np[0]["spec"].get("ingress", []):
        for p in rule.get("ports", []):
            ports.add(p.get("port"))
    check(ports == {9091},
          f"[base] payment-admin-ingress must allow only port 9091, got {ports}")

# =====================================================================
# DEV OVERLAY: runnable stateful topology.
# =====================================================================
dev_sts   = [d for d in dev_docs if d.get("kind") == "StatefulSet"]
dev_secrets = [d for d in dev_docs if d.get("kind") == "Secret"]
dev_svcs  = [d for d in dev_docs if d.get("kind") == "Service"]
dev_deploy = {d["metadata"]["name"]: d for d in dev_docs if d.get("kind") == "Deployment"}

sts_names = sorted(d["metadata"]["name"] for d in dev_sts)

# --- Seven PostgreSQL StatefulSets + one RabbitMQ StatefulSet ---
check(len(dev_sts) == 8,
      f"[dev] expected 8 StatefulSets (7 PG + 1 RMQ), got {len(dev_sts)}: {sts_names}")
for db in PG_DBS:
    check(db in {s["metadata"]["name"] for s in dev_sts},
          f"[dev] missing PostgreSQL StatefulSet '{db}'")
check("rabbitmq" in {s["metadata"]["name"] for s in dev_sts},
      "[dev] missing RabbitMQ StatefulSet")

# Each StatefulSet: one replica, a PVC template, a headless Service, dev label.
dev_svc_names = {s["metadata"]["name"] for s in dev_svcs}
for sts in dev_sts:
    name = sts["metadata"]["name"]
    check(sts["spec"].get("replicas") == 1,
          f"[dev] {name}: expected replicas=1, got {sts['spec'].get('replicas')}")
    check(sts["spec"].get("serviceName") == name,
          f"[dev] {name}: serviceName must match the StatefulSet name")
    vc = sts["spec"].get("volumeClaimTemplates", [])
    check(len(vc) >= 1,
          f"[dev] {name}: expected at least one volumeClaimTemplate, got {len(vc)}")
    # Headless Service with the same name must exist.
    svcmatch = [s for s in dev_svcs if s["metadata"]["name"] == name]
    check(len(svcmatch) == 1,
          f"[dev] {name}: expected a headless Service named '{name}'")
    if svcmatch:
        check(svcmatch[0]["spec"].get("clusterIP") == "None",
              f"[dev] {name}: headless Service must have clusterIP=None")
    # Dev-only unsafe label on both the StatefulSet and its pod template.
    lbl = sts["metadata"].get("labels", {})
    check(lbl.get("bank.jiade/unsafe") == "dev-only",
          f"[dev] {name}: StatefulSet missing bank.jiade/unsafe=dev-only label")
    pod_lbl = sts["spec"]["template"]["metadata"].get("labels", {})
    check(pod_lbl.get("bank.jiade/unsafe") == "dev-only",
          f"[dev] {name}: pod template missing bank.jiade/unsafe=dev-only label")

# PostgreSQL StatefulSets carry POSTGRES_DB matching the compose DB_NAME.
EXPECTED_DB = {"core-banking-db": "core_db", "customer-db": "cust_db",
               "payment-db": "pay_db", "reward-db": "reward_db",
               "risk-db": "risk_db", "loan-db": "loan_db",
               "wealth-db": "wealth_db"}
for sts in dev_sts:
    name = sts["metadata"]["name"]
    if name not in EXPECTED_DB:
        continue
    env = {e["name"]: e.get("value") for e in
           sts["spec"]["template"]["spec"]["containers"][0].get("env", [])}
    check(env.get("POSTGRES_DB") == EXPECTED_DB[name],
          f"[dev] {name}: expected POSTGRES_DB={EXPECTED_DB[name]}, "
          f"got {env.get('POSTGRES_DB')}")

# --- Dev Secret carries the three application keys via stringData ---
check(len(dev_secrets) == 1,
      f"[dev] expected exactly one Secret, got {len(dev_secrets)}")
if dev_secrets:
    sec = dev_secrets[0]
    check(sec["metadata"]["name"] == "bank-dev-secrets",
          f"[dev] Secret name mismatch: {sec['metadata']['name']}")
    check(sec["metadata"].get("labels", {}).get("bank.jiade/unsafe") == "dev-only",
          "[dev] Secret missing bank.jiade/unsafe=dev-only label")
    keys = set(sec.get("stringData", {}) or sec.get("data", {}))
    for k in ["DB_PASSWORD", "BROKER_URL", "BANK_OPERATOR_TOKEN"]:
        check(k in keys, f"[dev] Secret missing key '{k}'")

# --- Every application Deployment pulls the dev Secret via envFrom ---
for name in SERVICES:
    d = dev_deploy.get(name)
    if not d:
        check(False, f"[dev] {name}: Deployment missing from dev render")
        continue
    envfrom = d["spec"]["template"]["spec"]["containers"][0].get("envFrom", [])
    refs = [e.get("secretRef", {}).get("name") for e in envfrom]
    check("bank-dev-secrets" in refs,
          f"[dev] {name}: envFrom missing secretRef bank-dev-secrets (got {refs})")

# --- Public Ingress has no gRPC or admin route (re-checked on the overlay) ---
dev_ingress_blob = yaml.safe_dump([d for d in dev_docs if d.get("kind") == "Ingress"])
check("9090" not in dev_ingress_blob,
      "[dev] Ingress must not reference gRPC/9090")
check("9091" not in dev_ingress_blob,
      "[dev] Ingress must not reference admin-gRPC/9091")

# =====================================================================
# PROD OVERLAY: external state, no plaintext credentials.
# =====================================================================
prod_sts  = [d for d in prod_docs if d.get("kind") == "StatefulSet"]
prod_sec  = [d for d in prod_docs if d.get("kind") == "Secret"]
prod_svcs = [d for d in prod_docs if d.get("kind") == "Service"]
prod_spc  = [d for d in prod_docs if d.get("kind") == "SecretProviderClass"]
prod_deploy = {d["metadata"]["name"]: d for d in prod_docs if d.get("kind") == "Deployment"}
prod_raw = open(prod_path).read()

# --- No StatefulSets, no PVCs, no Secrets, no stringData ---
check(len(prod_sts) == 0,
      f"[prod] expected ZERO StatefulSets, got {len(prod_sts)}: "
      f"{names_of(prod_docs, 'StatefulSet')}")
check(len(prod_sec) == 0,
      f"[prod] expected ZERO Secrets, got {len(prod_sec)}: "
      f"{names_of(prod_docs, 'Secret')}")
check("stringData" not in prod_raw,
      "[prod] rendered manifests must NOT contain the literal 'stringData'")

# --- SecretProviderClass contract present, no credential values ---
check(len(prod_spc) == 1,
      f"[prod] expected one SecretProviderClass, got {len(prod_spc)}")
if prod_spc:
    spc_name = prod_spc[0]["metadata"]["name"]
    check(spc_name == "bank-prod-secrets",
          f"[prod] SecretProviderClass name mismatch: {spc_name}")
    # The secretObjects stanza must project the three application keys.
    so = prod_spc[0]["spec"].get("secretObjects", [])
    projected = set()
    for s in so:
        for item in s.get("data", []):
            projected.add(item.get("key"))
    for k in ["DB_PASSWORD", "BROKER_URL", "BANK_OPERATOR_TOKEN"]:
        check(k in projected,
              f"[prod] SecretProviderClass must project key '{k}'")

# --- Eight ExternalName Services for the data plane ---
ext = {s["metadata"]["name"]: s for s in prod_svcs
       if s["spec"].get("type") == "ExternalName"}
expected_ext = set(PG_DBS) | {"rabbitmq"}
check(set(ext) == expected_ext,
      f"[prod] ExternalName Service names mismatch: "
      f"got {sorted(ext)}, expected {sorted(expected_ext)}")
for name, svc in ext.items():
    en = svc["spec"].get("externalName", "")
    check(en != "" and en != "REPLACE-ME",
          f"[prod] {name}: externalName must be a non-empty configured DNS name, "
          f"got '{en}'")

# --- Every application Deployment references the prod Secret via envFrom ---
for name in SERVICES:
    d = prod_deploy.get(name)
    if not d:
        check(False, f"[prod] {name}: Deployment missing from prod render")
        continue
    envfrom = d["spec"]["template"]["spec"]["containers"][0].get("envFrom", [])
    refs = [e.get("secretRef", {}).get("name") for e in envfrom]
    check("bank-prod-secrets" in refs,
          f"[prod] {name}: envFrom missing secretRef bank-prod-secrets (got {refs})")

# --- Public Ingress has no gRPC or admin route (re-checked on the overlay) ---
prod_ingress_blob = yaml.safe_dump([d for d in prod_docs if d.get("kind") == "Ingress"])
check("9090" not in prod_ingress_blob,
      "[prod] Ingress must not reference gRPC/9090")
check("9091" not in prod_ingress_blob,
      "[prod] Ingress must not reference admin-gRPC/9091")

# =====================================================================
# Report
# =====================================================================
if errors:
    print(f"FAIL: {len(errors)} assertion(s) failed:", file=sys.stderr)
    for e in errors:
        print(f"  - {e}", file=sys.stderr)
    sys.exit(1)

print(f"OK: base {len(base_docs)} docs "
      f"({len(deployments)} Deploy, {len(services)} Svc, {len(netpol_base)} NetPol); "
      f"dev {len(dev_docs)} docs ({len(dev_sts)} STS); "
      f"prod {len(prod_docs)} docs ({len(prod_sts)} STS, {len(ext)} ExternalName)")
PY
