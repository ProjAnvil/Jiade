.PHONY: generate test bank-test bank-ci commerce-ci dcn-ci e2e clean

# Pack all built-in templates → templates.tar (required by go:embed)
generate:
	go generate ./internal/template

# jiade itself (without templates/bank - it is an independent module)
test: generate
	go build ./...
	go test ./...

# bank template multi-project verification (acceptance #2)
bank-test:
	cd templates/bank && $(MAKE) build test

# bank template static CI mirror (per-project build, test, coverage, race on
# the shared lib, config/observability/topology checks, k8s kustomize dev/prod
# render, proto regen diff). Mirrors the bank job in .github/workflows/ci.yml.
bank-ci:
	cd templates/bank && $(MAKE) build test cover race config-check observability-check topology-test
	kubectl kustomize templates/bank/deploy/k8s/overlays/dev >/tmp/bank-dev.yaml
	kubectl kustomize templates/bank/deploy/k8s/overlays/prod >/tmp/bank-prod.yaml
	cd templates/bank && $(MAKE) proto && git diff --exit-code -- templates/bank/contracts/gen

# commerce template static verification (per-project build, test, coverage,
# compose, and manifests)
commerce-ci:
	cd templates/commerce && $(MAKE) build test cover config-check
	kubectl kustomize templates/commerce/deploy/k8s >/tmp/commerce-k8s.yaml

# dcn template static verification (multi-module build, test, coverage, compose topology)
dcn-ci:
	cd templates/bank_dcn && $(MAKE) build test cover topology-test

# End-to-end smoke (requires docker; acceptance #4/#5).
# Generates a bank project from the embedded template, boots the full
# multi-database + Traefik topology, and runs the 10-gate smoke suite.
e2e: generate
	rm -rf /tmp/jiade-e2e
	go run ./cmd/jiade init --template bank --dir /tmp/jiade-e2e --force
	cd /tmp/jiade-e2e && make up
	cd /tmp/jiade-e2e && make smoke
	@echo "E2E OK"

clean:
	rm -rf /tmp/jiade-e2e
