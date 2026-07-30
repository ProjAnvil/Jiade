#!/usr/bin/env bash
# Render the bank Kubernetes application base and assert its shape.
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

if [ ! -d "${base_dir}" ]; then
  echo "FAIL: base directory not found: ${base_dir}" >&2
  exit 2
fi

rendered="$(mktemp -t bank-k8s-XXXXXX.yaml)"
trap 'rm -f "${rendered}"' EXIT

echo "rendering ${base_dir}"
kubectl kustomize "${base_dir}" >"${rendered}"

python3 - "${rendered}" <<'PY'
import sys, yaml

path = sys.argv[1]
with open(path) as f:
    docs = [d for d in yaml.safe_load_all(f) if d]

def names_of(kind):
    return [d["metadata"]["name"] for d in docs if d.get("kind") == kind]

deployments = [d for d in docs if d.get("kind") == "Deployment"]
hpas        = [d for d in docs if d.get("kind") == "HorizontalPodAutoscaler"]
pdbs        = [d for d in docs if d.get("kind") == "PodDisruptionBudget"]
services    = [d for d in docs if d.get("kind") == "Service"]
ingresses   = [d for d in docs if d.get("kind") == "Ingress"]
configmaps  = [d for d in docs if d.get("kind") == "ConfigMap"]
namespaces  = [d for d in docs if d.get("kind") == "Namespace"]

svc_names   = {s["metadata"]["name"] for s in services}

errors = []
def check(cond, msg):
    if not cond:
        errors.append(msg)

SERVICES = ["core-banking", "customer", "payment", "reward", "risk", "loan", "wealth"]
REPLICAS = {"core-banking": 2, "customer": 1, "payment": 2,
            "reward": 1, "risk": 2, "loan": 1, "wealth": 1}

# --- Exact counts (brief Step 1) ---
check(len(deployments) == 7,
      f"expected 7 Deployments, got {len(deployments)} ({names_of('Deployment')})")
check(len(hpas) == 7,
      f"expected 7 HorizontalPodAutoscalers, got {len(hpas)} ({names_of('HorizontalPodAutoscaler')})")
check(len(pdbs) == 3,
      f"expected 3 PodDisruptionBudgets, got {len(pdbs)} ({names_of('PodDisruptionBudget')})")

# --- Namespace present ---
check(any(ns["metadata"]["name"] == "bank" for ns in namespaces),
      "expected a Namespace named 'bank'")

# --- Each Deployment: name, ports, replicas, command ---
deploy_by_name = {d["metadata"]["name"]: d for d in deployments}
check(set(deploy_by_name) == set(SERVICES),
      f"Deployment names mismatch: {sorted(deploy_by_name)}")

for name in SERVICES:
    d = deploy_by_name.get(name)
    if not d:
        continue
    spec = d["spec"]["template"]["spec"]
    ctrs = spec.get("containers", [])
    check(len(ctrs) == 1, f"{name}: expected exactly 1 container, got {len(ctrs)}")
    if not ctrs:
        continue
    c = ctrs[0]
    # Ports http=8080, grpc=9090
    ports = {p.get("name"): p.get("containerPort") for p in c.get("ports", [])}
    check(ports.get("http") == 8080,
          f"{name}: expected containerPort http=8080, got {ports}")
    check(ports.get("grpc") == 9090,
          f"{name}: expected containerPort grpc=9090, got {ports}")
    # Replica defaults match the Global Constraints table
    check(d["spec"].get("replicas") == REPLICAS[name],
          f"{name}: expected replicas={REPLICAS[name]}, got {d['spec'].get('replicas')}")
    # Shared image referenced by all Deployments
    img = c.get("image", "")
    check(img != "", f"{name}: container image is empty")
    # command mirrors compose entrypoint
    cmd = c.get("command")
    check(cmd == ["/usr/local/bin/service"],
          f"{name}: expected command=['/usr/local/bin/service'], got {cmd}")

# All Deployments reference the SAME image (single shared binary image).
images = {d["spec"]["template"]["spec"]["containers"][0]["image"] for d in deployments}
check(len(images) == 1,
      f"expected all Deployments to share one image, got {sorted(images)}")

# --- Service shapes: REST ClusterIP + headless <name>-grpc ---
for name in SERVICES:
    rest = [s for s in services if s["metadata"]["name"] == name]
    check(len(rest) == 1,
          f"{name}: expected one REST Service named '{name}', got {len(rest)}")
    if rest:
        rspec = rest[0]["spec"]
        # REST service must NOT be headless
        check(rspec.get("clusterIP") != "None",
              f"{name}: REST Service must not be headless (clusterIP=None)")
        # Must target the http port
        tports = {p.get("name"): p.get("port") for p in rspec.get("ports", [])}
        check(tports.get("http") == 8080,
              f"{name}: REST Service must expose port http=8080, got {tports}")

    grpc_name = f"{name}-grpc"
    grpc = [s for s in services if s["metadata"]["name"] == grpc_name]
    check(len(grpc) == 1,
          f"{name}: expected one headless Service '{grpc_name}', got {len(grpc)}")
    if grpc:
        gspec = grpc[0]["spec"]
        check(gspec.get("clusterIP") == "None",
              f"{grpc_name}: expected clusterIP=None, got {gspec.get('clusterIP')}")
        check(gspec.get("publishNotReadyAddresses") is False,
              f"{grpc_name}: expected publishNotReadyAddresses=false, "
              f"got {gspec.get('publishNotReadyAddresses')}")
        tports = {p.get("name"): p.get("port") for p in gspec.get("ports", [])}
        check(tports.get("grpc") == 9090,
              f"{grpc_name}: headless Service must expose port grpc=9090, got {tports}")
        check(gspec.get("selector") is not None,
              f"{grpc_name}: headless Service must have a selector")

# No extra Service beyond the 14 expected (7 REST + 7 grpc).
check(len(services) == 14,
      f"expected exactly 14 Services (7 REST + 7 grpc), got {len(services)}: {sorted(svc_names)}")

# --- Ingress: only public REST prefixes, targets only REST Services ---
ingress_blob = yaml.safe_dump(ingresses)
check("/internal/" not in ingress_blob,
      "Ingress must not route any /internal/ prefix")
check("9090" not in ingress_blob,
      "Ingress must not reference the gRPC/9090 port")
check(len(ingresses) >= 1,
      f"expected at least one Ingress, got {len(ingresses)}")

for ing in ingresses:
    for rule in ing["spec"].get("rules", []):
        paths = rule.get("http", {}).get("paths", [])
        for p in paths:
            be = p.get("backend", {})
            svc_name = be.get("service", {}).get("name", "")
            sport = (be.get("service", {}).get("port", {}) or {})
            port_name = sport.get("name")
            port_num  = sport.get("number")
            # Target only REST Services and only the http port.
            check(svc_name in SERVICES,
                  f"Ingress backend '{svc_name}' is not a REST Service")
            check(port_name == "http" or port_num == 8080,
                  f"Ingress backend for '{svc_name}' targets {port_name or port_num}, "
                  "expected http/8080")
            # Paths must be public /api/v1/... prefixes only.
            pp = p.get("path", "")
            check(pp.startswith("/api/v1/"),
                  f"Ingress path '{pp}' is not a public /api/v1/* prefix")

# --- Availability: PDBs for multi-replica only, HPAs for all seven ---
pdb_names = {p["metadata"]["name"] for p in pdbs}
check(pdb_names == {"core-banking", "payment", "risk"},
      f"PDB names mismatch (expected core-banking/payment/risk): {sorted(pdb_names)}")

hpa_names = {h["metadata"]["name"] for h in hpas}
check(hpa_names == set(SERVICES),
      f"HPA names mismatch: {sorted(hpa_names)}")

for h in hpas:
    ref = h["spec"]["scaleTargetRef"]
    check(ref.get("kind") == "Deployment",
          f"{h['metadata']['name']}: HPA must target a Deployment, got {ref.get('kind')}")
    # CPU-based HPA only
    metrics = h["spec"].get("metrics", [])
    check(all(m.get("type") == "Resource" for m in metrics),
          f"{h['metadata']['name']}: HPA must use Resource metrics only, got {metrics}")
    check(any(m.get("resource", {}).get("name") == "cpu" for m in metrics),
          f"{h['metadata']['name']}: HPA must have a cpu resource metric")

for p in pdbs:
    ref = p["spec"]["selector"]
    check("matchLabels" in ref,
          f"{p['metadata']['name']}: PDB must use a label selector")

# --- ConfigMap: non-secret defaults, no production secrets ---
cms = [c for c in configmaps if c["metadata"]["name"] == "bank-config"]
check(len(cms) == 1,
      f"expected exactly one ConfigMap 'bank-config', got {len(cms)}")
if cms:
    cm_blob = yaml.safe_dump(cms[0])
    check("DB_PASSWORD" not in cm_blob,
          "ConfigMap must NOT contain DB_PASSWORD (secret)")
    check("BROKER_URL" not in cm_blob or "://" not in cm_blob or
          ("bank:bank@" not in cm_blob and ":@" not in cm_blob),
          "ConfigMap must NOT embed broker credentials")

# All Deployments must carry the common app labels for selection.
for name in SERVICES:
    d = deploy_by_name[name]
    labels = d["metadata"].get("labels", {})
    check(labels.get("app.kubernetes.io/name") == "bank",
          f"{name}: Deployment missing app.kubernetes.io/name=bank label")
    tmpl_labels = d["spec"]["template"]["metadata"].get("labels", {})
    check(tmpl_labels.get("app.kubernetes.io/name") == "bank",
          f"{name}: pod template missing app.kubernetes.io/name=bank label")

# --- Report ---
if errors:
    print(f"FAIL: {len(errors)} assertion(s) failed:", file=sys.stderr)
    for e in errors:
        print(f"  - {e}", file=sys.stderr)
    sys.exit(1)

print(f"OK: rendered {len(docs)} document(s); "
      f"{len(deployments)} Deployment, {len(services)} Service, "
      f"{len(ingresses)} Ingress, {len(pdbs)} PDB, {len(hpas)} HPA")
PY
