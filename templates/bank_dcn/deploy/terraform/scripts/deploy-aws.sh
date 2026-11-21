#!/usr/bin/env bash
# Deployment stage for the AWS bank_dcn stack (the third WeCube-style stage:
# planning -> provisioning -> deployment). Run from anywhere inside the
# generated project after `terraform apply` succeeded in deploy/terraform:
#
#   deploy/terraform/scripts/deploy-aws.sh [--push-images] [--verify]
#
# Prerequisites: awscli with EC2/RDS/ELB/SSM/IAM/ECR permissions, docker
# (for --push-images), python3. All node scripts are generated from
# generated/dcn-topology.json — no hardcoded IPs; re-running after an
# infrastructure change (new unit, recreated instance) refreshes routes,
# gateway targets, and scrape configs accordingly.
set -euo pipefail

TF_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ROOT_DIR="$(cd "$TF_DIR/../.." && pwd)"
cd "$TF_DIR"

PUSH_IMAGES=0
VERIFY=0
for arg in "$@"; do
  case "$arg" in
    --push-images) PUSH_IMAGES=1 ;;
    --verify) VERIFY=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

TOPO="$TF_DIR/generated/dcn-topology.json"
[ -f "$TOPO" ] || { echo "missing $TOPO — run terraform apply first" >&2; exit 1; }

tq() { python3 -c "import json,sys;d=json.load(open('$TOPO'));print($1)"; }

REGION=$(tq 'd["region"]')
SVC_IP=$(tq 'd["global_zone"]["sf_private_ips"][0]')
MQ_IP=$(tq 'd["global_zone"]["mq_private_ip"]')
ALB_DNS=$(tq 'd["global_zone"]["ingress_lb_dns"]')
UNITS=$(tq '" ".join(sorted(d["units"].keys()))')
ACCT=$(aws sts get-caller-identity --query Account --output text)
REG="$ACCT.dkr.ecr.$REGION.amazonaws.com"
PW="${DB_MASTER_PASSWORD:-$(grep -oE 'db_master_password[[:space:]]*=[[:space:]]*"[^"]+"' terraform.tfvars | sed 's/.*"\(.*\)"/\1/')}"
[ -n "$PW" ] || { echo "set DB_MASTER_PASSWORD or db_master_password in terraform.tfvars" >&2; exit 1; }
export AWS_REGION="$REGION"

echo "== topology: region=$REGION units=[$UNITS] svc=$SVC_IP mq=$MQ_IP alb=$ALB_DNS"

ssm_run() { # <instance-id> <script-file>
  local id=$1 f=$2
  local b64; b64=$(base64 -i "$f" | tr -d '\n')
  local cmd; cmd=$(aws ssm send-command --document-name AWS-RunShellScript \
    --targets "Key=InstanceIds,Values=$id" \
    --parameters "commands=[\"echo $b64 | base64 -d | bash 2>&1 | tail -30\"]" \
    --query 'Command.CommandId' --output text)
  for _ in $(seq 1 40); do
    sleep 15
    local st; st=$(aws ssm list-command-invocations --command-id "$cmd" --query 'CommandInvocations[0].StatusDetails' --output text)
    case "$st" in Pending|InProgress) continue ;; esac
    aws ssm list-command-invocations --command-id "$cmd" --details \
      --query 'CommandInvocations[0].CommandPlugins[0].Output' --output text
    [ "$st" = "Success" ]
    return
  done
  echo "timeout waiting for SSM command $cmd" >&2; return 1
}

instance_id() { aws ec2 describe-instances --filters "Name=tag:Name,Values=$1" \
  --query 'Reservations[].Instances[?State.Name!=`terminated`].InstanceId' --output text | awk '{print $1}'; }

# ---------- 0. images ----------
if [ "$PUSH_IMAGES" = "1" ]; then
  echo "== building and pushing images"
  aws ecr describe-repositories --repository-names bank-dcn >/dev/null 2>&1 \
    || aws ecr create-repository --repository-name bank-dcn >/dev/null
  aws ecr get-login-password | docker login --username AWS --password-stdin "$REG" >/dev/null
  for s in gns rmb dcn-app adm batch-scheduler console; do
    docker buildx build --platform linux/amd64 --load -q -t "bank-dcn/$s:aws" -f "$ROOT_DIR/$s/Dockerfile" "$ROOT_DIR" >/dev/null
    docker tag "bank-dcn/$s:aws" "$REG/bank-dcn:$s"
    docker push -q "$REG/bank-dcn:$s" >/dev/null && echo "pushed $s"
  done
fi

# ---------- 1. SSM role + outbound internet (idempotent) ----------
echo "== IAM role and instance profile"
aws iam get-role --role-name dcn-ssm-ecr >/dev/null 2>&1 || {
  aws iam create-role --role-name dcn-ssm-ecr \
    --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}' >/dev/null
  aws iam attach-role-policy --role-name dcn-ssm-ecr --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
  aws iam put-role-policy --role-name dcn-ssm-ecr --policy-name ecr-pull \
    --policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["ecr:GetAuthorizationToken","ecr:BatchGetImage","ecr:GetDownloadUrlForLayer","ecr:BatchCheckLayerAvailability"],"Resource":"*"}]}' >/dev/null
  sleep 10
}
aws iam get-instance-profile --instance-profile-name dcn-ssm-ecr >/dev/null 2>&1 || {
  aws iam create-instance-profile --instance-profile-name dcn-ssm-ecr >/dev/null; sleep 10
  aws iam add-role-to-instance-profile --instance-profile-name dcn-ssm-ecr --role-name dcn-ssm-ecr
}

echo "== IGW, default route, EIPs"
VPC=$(aws ec2 describe-vpcs --filters "Name=tag:Name,Values=dcn-vpc" --query 'Vpcs[0].VpcId' --output text)
IGW=$(aws ec2 describe-internet-gateways --filters "Name=attachment.vpc-id,Values=$VPC" --query 'InternetGateways[0].InternetGatewayId' --output text)
if [ "$IGW" = "None" ]; then
  IGW=$(aws ec2 create-internet-gateway --query 'InternetGateway.InternetGatewayId' --output text)
  aws ec2 attach-internet-gateway --internet-gateway-id "$IGW" --vpc-id "$VPC"
fi
MAINRT=$(aws ec2 describe-route-tables --filters "Name=vpc-id,Values=$VPC" "Name=association.main,Values=true" --query 'RouteTables[0].RouteTableId' --output text)
aws ec2 create-route --route-table-id "$MAINRT" --destination-cidr-block 0.0.0.0/0 --gateway-id "$IGW" >/dev/null 2>&1 || true

NODES=""
for name in $(for u in $UNITS; do echo "dcn-$u-app-1"; done) dcn-global-svc-1 dcn-global-mgmt-1 dcn-global-mq-1; do
  id=$(instance_id "$name"); [ -z "$id" ] && continue
  NODES="$NODES $id"
  aws ec2 describe-iam-instance-profile-associations --filters "Name=instance-id,Values=$id" \
    --query 'length(IamInstanceProfileAssociations)' --output text | grep -q '^1$' \
    || aws ec2 associate-iam-instance-profile --instance-id "$id" --iam-instance-profile Name=dcn-ssm-ecr >/dev/null 2>&1 || true
  PUB=$(aws ec2 describe-instances --instance-ids "$id" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
  if [ -z "$PUB" ] || [ "$PUB" = "None" ]; then
    EIP=$(aws ec2 allocate-address --domain vpc --query AllocationId --output text)
    aws ec2 associate-address --instance-id "$id" --allocation-id "$EIP" >/dev/null && echo "eip -> $name"
  fi
done

echo "== rebooting nodes for a clean SSM agent registration"
aws ec2 reboot-instances --instance-ids $NODES
sleep 150
for id in $NODES; do
  for _ in $(seq 1 10); do
    aws ssm describe-instance-information --filters "Key=InstanceIds,Values=$id" \
      --query 'InstanceInformationList[0].PingStatus' --output text | grep -q Online && break
    sleep 15
  done
done

# ---------- 2. security group rules (idempotent) ----------
echo "== security group rules"
SFSG=$(aws ec2 describe-security-groups --filters "Name=group-name,Values=dcn-sg-sf" --query 'SecurityGroups[0].GroupId' --output text)
for p in 6379 15692 18070 18080 18090 18091 18092; do
  aws ec2 authorize-security-group-ingress --group-id "$SFSG" --protocol tcp --port "$p" --cidr 10.60.0.0/16 >/dev/null 2>&1 || true
done
MGSG=$(aws ec2 describe-security-groups --filters "Name=group-name,Values=dcn-sg-mgmt" --query 'SecurityGroups[0].GroupId' --output text)
aws ec2 authorize-security-group-ingress --group-id "$MGSG" --protocol tcp --port 18099 --cidr 0.0.0.0/0 >/dev/null 2>&1 || true

# ---------- 3. node scripts, all rendered from the live topology ----------
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

cat > "$WORK/node-setup.sh" <<EOF
#!/bin/bash
set -euo pipefail
command -v docker >/dev/null 2>&1 || { dnf install -y docker >/dev/null 2>&1; systemctl enable --now docker; }
command -v aws >/dev/null 2>&1 || dnf install -y aws-cli >/dev/null 2>&1
aws ecr get-login-password --region $REGION | docker login --username AWS --password-stdin $REG >/dev/null 2>&1
for s in gns rmb adm batch-scheduler console dcn-app; do docker pull -q $REG/bank-dcn:\$s >/dev/null; done
docker network create dcn 2>/dev/null || true
echo node-ready
EOF

cat > "$WORK/mq.sh" <<'EOF'
#!/bin/bash
set -euo pipefail
mkdir -p /etc/rabbitmq
printf '[rabbitmq_management,rabbitmq_prometheus].\n' > /etc/rabbitmq/enabled_plugins
docker rm -f dcn-rabbitmq gns-redis 2>/dev/null
docker run -d --name dcn-rabbitmq --restart unless-stopped --network dcn \
  -p 5672:5672 -p 15672:15672 -p 15692:15692 \
  -e RABBITMQ_DEFAULT_USER=dcn -e RABBITMQ_DEFAULT_PASS=dcn123 \
  -v /etc/rabbitmq/enabled_plugins:/etc/rabbitmq/enabled_plugins:ro rabbitmq:3.13-management >/dev/null
docker run -d --name gns-redis --restart unless-stopped --network dcn -p 6379:6379 redis:7-alpine >/dev/null
sleep 5; docker ps --format '{{.Names}} | {{.Status}}'
EOF

python3 - "$WORK/svc.sh" "$TOPO" "$ROOT_DIR" "$PW" "$MQ_IP" "$REG" <<'PY'
import json, sys
out, topo, root, pw, mq, reg = sys.argv[1:7]
t = json.load(open(topo))
g = t["global_zone"]["db_endpoints"]
units = sorted(t["units"].items())

def sql(path):
    return open(f"{root}/{path}/db/init/01_init.sql").read()

# GNS seed: route endpoints derive from the live topology, never hardcoded.
base = open(f"{root}/gns/db/init/01_init.sql").read().split("INSERT INTO route_segment")[0]
rows = []
for i, (name, u) in enumerate(units):
    s = 1000 + 1000 * i
    rows.append(f"  ('{name}', {s}, {s + 999}, 'http://{u['app_private_ips'][0]}:8080', 'ACTIVE')")
gns_sql = base + "INSERT INTO route_segment (dcn, seg_start, seg_end, endpoint, status) VALUES\n" + ",\n".join(rows) + ";"

sqls = {"gns": gns_sql, "rmb": sql("rmb"), "adm": sql("adm"), "batch": sql("batch-scheduler"), "dcn": sql("dcn-app")}
emb = "".join(f"cat > /opt/dcn/sql/{k}.sql <<'SQL'\n{v}\nSQL\n" for k, v in sqls.items())

def dsn(ep, db):
    return f"dcnadmin:{pw}@tcp({ep.rsplit(':', 1)[0]}:3306)/{db}?parseTime=true"

servers = "\n".join(f"          - {{url: \"http://{u['app_private_ips'][0]}:8080\"}}" for _, u in units)
dyn = f"""cat > /opt/dcn/traefik/dynamic.yml <<'YEOF'
http:
  middlewares:
    strip:
      stripPrefix:
        prefixes: ["/gns", "/rmb", "/adm", "/batch", "/dcn"]
  routers:
    gns:   {{rule: "PathPrefix(`/gns`)",   service: gns,   middlewares: [strip]}}
    rmb:   {{rule: "PathPrefix(`/rmb`)",   service: rmb,   middlewares: [strip]}}
    adm:   {{rule: "PathPrefix(`/adm`)",   service: adm,   middlewares: [strip]}}
    batch: {{rule: "PathPrefix(`/batch`)", service: batch, middlewares: [strip]}}
    dcn:   {{rule: "PathPrefix(`/dcn`)",   service: dcn,   middlewares: [strip]}}
  services:
    gns:   {{loadBalancer: {{servers: [{{url: "http://gns:8080"}}]}}}}
    rmb:   {{loadBalancer: {{servers: [{{url: "http://rmb-coordinator:8080"}}]}}}}
    adm:   {{loadBalancer: {{servers: [{{url: "http://adm:8080"}}]}}}}
    batch: {{loadBalancer: {{servers: [{{url: "http://batch-scheduler:8080"}}]}}}}
    dcn:
      loadBalancer:
        servers:
{servers}
YEOF"""

script = f"""#!/bin/bash
set -uo pipefail
mkdir -p /opt/dcn/sql /opt/dcn/traefik
{emb}
init_db() {{
  local db=$1 host=$2 sql=$3
  docker run --rm mysql:8.0 mysql -h "$host" -u dcnadmin -p'{pw}' -e "DROP DATABASE IF EXISTS \\`$db\\`; CREATE DATABASE \\`$db\\`;" 2>/dev/null
  docker run --rm -i mysql:8.0 mysql -h "$host" -u dcnadmin -p'{pw}' "$db" < "$sql" 2>/dev/null || echo "INIT-FAIL $db"
}}
init_db gns_db   "{g['gns']}"   /opt/dcn/sql/gns.sql
init_db rmb_db   "{g['rmb']}"   /opt/dcn/sql/rmb.sql
init_db adm_db   "{g['adm']}"   /opt/dcn/sql/adm.sql
init_db batch_db "{g['batch']}" /opt/dcn/sql/batch.sql
"""
for name, u in units:
    script += f'init_db {name}_db "{u["db_endpoint"]}" /opt/dcn/sql/dcn.sql\n'
script += f"""docker rm -f gns rmb-coordinator adm batch-scheduler dcn-traefik 2>/dev/null
docker run -d --name gns --restart unless-stopped --network dcn -p 18080:8080 \\
  -e "DB_DSN={dsn(g['gns'], 'gns_db')}" -e "REDIS_ADDR={mq}:6379" {reg}/bank-dcn:gns >/dev/null
docker run -d --name rmb-coordinator --restart unless-stopped --network dcn -p 18090:8080 \\
  -e "DB_DSN={dsn(g['rmb'], 'rmb_db')}" -e "AMQP_URL=amqp://dcn:dcn123@{mq}:5672/" \\
  -e "TX_TIMEOUT_SECONDS=5" {reg}/bank-dcn:rmb >/dev/null
docker run -d --name adm --restart unless-stopped --network dcn -p 18091:8080 \\
  -e "DB_DSN={dsn(g['adm'], 'adm_db')}" -e "AMQP_URL=amqp://dcn:dcn123@{mq}:5672/" \\
  -e "GNS_ENDPOINT=http://gns:8080" {reg}/bank-dcn:adm >/dev/null
docker run -d --name batch-scheduler --restart unless-stopped --network dcn -p 18092:8080 \\
  -e "DB_DSN={dsn(g['batch'], 'batch_db')}" -e "GNS_ENDPOINT=http://gns:8080" {reg}/bank-dcn:batch-scheduler >/dev/null
{dyn}
docker run -d --name dcn-traefik --restart unless-stopped --network dcn -p 18070:80 \\
  -v /opt/dcn/traefik/dynamic.yml:/etc/traefik/dynamic.yml:ro \\
  traefik:v3.6 --providers.file.filename=/etc/traefik/dynamic.yml --entryPoints.web.address=:80 >/dev/null
sleep 8
docker ps --format '{{{{.Names}}}} | {{{{.Status}}}}'
for p in 18080 18090 18091 18092; do curl -s -o /dev/null -w "port $p: %{{http_code}}\\n" --max-time 5 http://127.0.0.1:$p/healthz; done
"""
open(out, "w").write(script)
PY

for u in $UNITS; do
  python3 - "$WORK/dcn-$u.sh" "$TOPO" "$u" "$PW" "$SVC_IP" "$MQ_IP" "$REG" <<'PY'
import json, sys
out, topo, u, pw, svc, mq, reg = sys.argv[1:8]
t = json.load(open(topo))
unit = t["units"][u]
host = unit["db_endpoint"].rsplit(":", 1)[0]
open(out, "w").write(f'''#!/bin/bash
set -euo pipefail
docker rm -f dcn-app 2>/dev/null
docker run -d --name dcn-app --restart unless-stopped -p 8080:8080 \\
  -e "DCN_ID={u}" \\
  -e "DB_DSN=dcnadmin:{pw}@tcp({host}:3306)/{u}_db?parseTime=true" \\
  -e "GNS_ENDPOINT=http://{svc}:18080" \\
  -e "RMB_ENDPOINT=http://{svc}:18090" \\
  -e "AMQP_URL=amqp://dcn:dcn123@{mq}:5672/" \\
  -e "RATE_LIMIT_RPS=200" -e "INTEREST_DAILY_RATE=0.0001" \\
  {reg}/bank-dcn:dcn-app >/dev/null
for i in $(seq 1 15); do sleep 5; C=$(curl -s -o /dev/null -w '%{{http_code}}' --max-time 3 http://127.0.0.1:8080/healthz); [ "$C" = "200" ] && break; done
curl -s -o /dev/null -w "{u} healthz: %{{http_code}}\\n" --max-time 5 http://127.0.0.1:8080/healthz
''')
PY
done

python3 - "$WORK/mgmt.sh" "$TOPO" "$REG" <<'PY'
import json, sys
out, topo, reg = sys.argv[1:4]
t = json.load(open(topo))
svc_ip = t["global_zone"]["sf_private_ips"][0]
mq_ip = t["global_zone"]["mq_private_ip"]
svc_t = "\n".join(f"          - {svc_ip}:{p}" for p in (18080, 18090, 18091, 18092))
unit_t = "\n".join(f"          - {u['app_private_ips'][0]}:8080" for _, u in sorted(t["units"].items()))
open(out, "w").write(f'''#!/bin/bash
set -euo pipefail
mkdir -p /opt/dcn/prom /opt/dcn/grafana/provisioning/datasources
cat > /opt/dcn/prom/prometheus.yml <<'PEOF'
global:
  scrape_interval: 15s
scrape_configs:
  - job_name: dcn-services
    static_configs:
      - targets:
{svc_t}
  - job_name: dcn-units
    static_configs:
      - targets:
{unit_t}
  - job_name: rabbitmq
    static_configs:
      - targets: ["{mq_ip}:15692"]
PEOF
cat > /opt/dcn/grafana/provisioning/datasources/prom.yml <<'DEOF'
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://127.0.0.1:9090
    isDefault: true
DEOF
docker rm -f prometheus grafana console 2>/dev/null
docker run -d --name prometheus --restart unless-stopped -p 9090:9090 -v /opt/dcn/prom/prometheus.yml:/etc/prometheus/prometheus.yml:ro prom/prometheus:v2.54.1 >/dev/null
docker run -d --name grafana --restart unless-stopped -p 3000:3000 \\
  -e GF_AUTH_ANONYMOUS_ENABLED=true -e GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer -e GF_AUTH_DISABLE_LOGIN_FORM=true \\
  -v /opt/dcn/grafana/provisioning:/etc/grafana/provisioning:ro grafana/grafana:11.2.0 >/dev/null
docker run -d --name console --restart unless-stopped -p 18099:8080 \\
  -e PROMETHEUS_URL=http://127.0.0.1:9090 -e DOCKER_SOCKET=/var/run/docker.sock \\
  -v /var/run/docker.sock:/var/run/docker.sock:ro {reg}/bank-dcn:console >/dev/null
sleep 20
curl -s http://127.0.0.1:9090/api/v1/targets | python3 -c "import json,sys;d=json.load(sys.stdin);[print(t['labels'].get('job'),t['scrapeUrl'],t['health']) for t in d['data']['activeTargets']]"
docker ps --format '{{{{.Names}}}} | {{{{.Status}}}}'
''')
PY

# ---------- 4. execute ----------
SVC_ID=$(instance_id dcn-global-svc-1)
MQ_ID=$(instance_id dcn-global-mq-1)
for id in $NODES; do echo "-- node-setup"; ssm_run "$id" "$WORK/node-setup.sh" | tail -1; done
echo "-- mq node";   ssm_run "$MQ_ID" "$WORK/mq.sh" | tail -2
echo "-- svc node";  ssm_run "$SVC_ID" "$WORK/svc.sh" | tail -6
for u in $UNITS; do
  id=$(instance_id "dcn-$u-app-1")
  echo "-- unit $u"; ssm_run "$id" "$WORK/dcn-$u.sh" | tail -2
done
MGMT_ID=$(instance_id dcn-global-mgmt-1)
if [ -n "$MGMT_ID" ]; then
  echo "-- mgmt node"; ssm_run "$MGMT_ID" "$WORK/mgmt.sh" | tail -10
fi

# ---------- 5. ALB listener ----------
echo "== ALB target group and listener"
ALBARN=$(aws elbv2 describe-load-balancers --names dcn-global-ingress --query 'LoadBalancers[0].LoadBalancerArn' --output text)
TGARN=$(aws elbv2 describe-target-groups --names dcn-ingress-svc --load-balancer-arn "$ALBARN" --query 'TargetGroups[0].TargetGroupArn' --output text 2>/dev/null || true)
if [ -z "$TGARN" ]; then
  TGARN=$(aws elbv2 create-target-group --name dcn-ingress-svc --protocol HTTP --port 18070 \
    --vpc-id "$VPC" --health-check-protocol HTTP --health-check-path /gns/routes \
    --health-check-interval-seconds 15 --query 'TargetGroups[0].TargetGroupArn' --output text)
fi
aws elbv2 register-targets --target-group-arn "$TGARN" --targets Id="$SVC_ID" >/dev/null 2>&1 || true
NL=$(aws elbv2 describe-listeners --load-balancer-arn "$ALBARN" --query 'length(Listeners)' --output text)
if [ "$NL" = "0" ]; then
  aws elbv2 create-listener --load-balancer-arn "$ALBARN" --protocol HTTP --port 80 \
    --default-actions Type=forward,TargetGroupArn="$TGARN" >/dev/null && echo "listener created"
fi

echo "== deployment complete. gateway: http://$ALB_DNS"

if [ "$VERIFY" = "1" ]; then
  MIP=$(aws ec2 describe-instances --instance-ids "$MGMT_ID" --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)
  GATEWAY="http://$ALB_DNS" PROM_URL="http://$MIP:9090" bash "$TF_DIR/verify-aws.sh"
fi
