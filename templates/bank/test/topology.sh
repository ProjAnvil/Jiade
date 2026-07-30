#!/usr/bin/env bash
set -euo pipefail

docker compose -f compose.yaml config --quiet

compose_json="$(mktemp)"
seed_json="$(mktemp)"
trap 'rm -f "$compose_json" "$seed_json"' EXIT
docker compose -f compose.yaml config --format json >"$compose_json"
docker compose -f compose.yaml --profile seed config --format json >"$seed_json"

jq -e '
  . as $topology |
  ["core-banking", "customer", "payment", "reward", "risk", "loan", "wealth"] as $apps |
  ["core-banking-db", "customer-db", "payment-db", "reward-db", "risk-db", "loan-db", "wealth-db"] as $dbs |
  ["pg-core-banking", "pg-customer", "pg-payment", "pg-reward", "pg-risk", "pg-loan", "pg-wealth"] as $volumes |
  (([.services | keys[] | select(endswith("-db"))] | sort) == ($dbs | sort)) and
  (([.volumes | keys[] | select(startswith("pg-"))] | sort) == ($volumes | sort)) and
  ([.services | to_entries[] | select(.value.ports != null) | .key] == ["traefik"]) and
  (.services["core-banking"].deploy.replicas == 2) and
  (.services.payment.deploy.replicas == 2) and
  (.services.risk.deploy.replicas == 2) and
  (all(["customer", "reward", "loan", "wealth"][]; $topology.services[.].deploy.replicas == 1)) and
  (all($apps[];
    $topology.services[.] as $service |
    $service.read_only == true and
    (($service.tmpfs | index("/tmp")) != null) and
    ($service.cap_drop == ["ALL"]) and
    ($service.security_opt | index("no-new-privileges:true")) != null and
    $service.stop_grace_period == "30s" and
    $service.mem_reservation == "134217728" and
    $service.mem_limit == "536870912" and
    $service.cpus == 1
  )) and
  (all($dbs[];
    $topology.services[.] as $database |
    ($database.environment.POSTGRES_USER == "bank") and
    ($database.environment.POSTGRES_PASSWORD == "bank") and
    (($database.volumes | length) == 1) and
    ($database.volumes[0].target == "/var/lib/postgresql/data") and
    ($database.volumes[0].source | startswith("pg-"))
  )) and
  ([ $apps[] as $service |
     .services[$service].labels | to_entries[] | select(.key | endswith(".rule")) | .value
   ] as $rules |
   ($rules | length == 7) and
   ($rules | all(test("PathPrefix\\(`/api/v1/"))) and
   ($rules | all(test("/internal/|9090") | not))
  ) and
  (.services.customer.environment.CORE_BANKING_GRPC_TARGET == "dns:///core-banking:9090") and
  (.services.payment.environment.CORE_BANKING_GRPC_TARGET == "dns:///core-banking:9090") and
  (.services.payment.environment.CUSTOMER_GRPC_TARGET == "dns:///customer:9090") and
  (all(["reward", "risk", "loan", "wealth"][];
    $topology.services[.].environment.CUSTOMER_GRPC_TARGET == "dns:///customer:9090"))
' "$compose_json" >/dev/null

jq -e '
  . as $defs |
  def queue($name): any(.queues[]; .name == $name and .durable == true and .auto_delete == false);
  def binding($source; $destination; $key):
    any(.bindings[]; .source == $source and .destination == $destination and .destination_type == "queue" and .routing_key == $key);
  def retry_queue($name; $source_exchange; $source_key):
    any(.queues[];
      .name == $name and .durable == true and
      .arguments["x-message-ttl"] == 2000 and
      .arguments["x-dead-letter-exchange"] == $source_exchange and
      .arguments["x-dead-letter-routing-key"] == $source_key
    );
  ([.exchanges[].name] | sort == ["bank.commands", "bank.dlx", "bank.events", "bank.retry"]) and
  [
    {source: "risk.commands", source_exchange: "bank.commands", retry: "risk.commands.retry", dlq: "risk.commands.dlq", dead: "risk.commands.dead"},
    {source: "core-banking.commands", source_exchange: "bank.commands", retry: "core-banking.commands.retry", dlq: "core-banking.commands.dlq", dead: "core-banking.commands.dead"},
    {source: "payment.workflow.events", source_exchange: "bank.events", retry: "payment-workflow.retry", dlq: "payment-workflow.dlq", dead: "payment-workflow.dead"},
    {source: "reward.payment-events", source_exchange: "bank.events", retry: "reward.payment-events.retry", dlq: "reward.payment-events.dlq", dead: "reward.payment-events.dead"}
  ] as $flows |
  all($flows[];
    . as $flow | $defs |
    queue($flow.source) and
    retry_queue($flow.retry; $flow.source_exchange; $flow.source) and
    queue($flow.dlq) and
    binding($flow.source_exchange; $flow.source; $flow.source) and
    binding("bank.retry"; $flow.retry; $flow.retry) and
    binding("bank.dlx"; $flow.dlq; $flow.dead)
  )
' deploy/rabbitmq/definitions.json >/dev/null

# `cmd/seed` reads migrations by a relative path; verify both the final-image
# assets and the profile-only job that selects all seven dedicated hosts.
bash test/seed-image.sh
jq -e '
  .services.seed.build.args.SERVICE == "seed" and
  .services.seed.environment.DEDICATED_DATABASES == "true" and
  ([.services.seed.environment | keys[] | select(startswith("DB_HOST_"))] | length == 7)
' "$seed_json" >/dev/null
