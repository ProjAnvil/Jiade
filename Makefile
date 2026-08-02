.PHONY: generate test bank-test bank-ci commerce-ci dcn-ci e2e clean

# Pack all built-in templates → templates.tar (required by go:embed)
generate:
	go generate ./internal/template

# jiade itself (without templates/bank - it is an independent module)
test: generate
	go build ./...
	go test ./...

# bank template as standalone module verification (acceptance #2)
bank-test:
	cd templates/bank && go build ./... && go test ./...

# bank template static CI mirror (build, test, race platform, config/observability
# checks, k8s kustomize dev/prod render). Mirrors the bank job in .github/workflows/ci.yml.
bank-ci:
	cd templates/bank && go build ./...
	cd templates/bank && go test ./...
	cd templates/bank && go test -race ./internal/platform/...
	cd templates/bank && $(MAKE) config-check observability-check
	kubectl kustomize templates/bank/deploy/k8s/overlays/dev >/tmp/bank-dev.yaml
	kubectl kustomize templates/bank/deploy/k8s/overlays/prod >/tmp/bank-prod.yaml

# commerce template static verification (build, test, compose, and manifests)
commerce-ci:
	cd templates/commerce && go build ./...
	cd templates/commerce && go test ./...
	cd templates/commerce && go test -race ./internal/platform/...
	cd templates/commerce && $(MAKE) config-check
	kubectl kustomize templates/commerce/deploy/k8s >/tmp/commerce-k8s.yaml

# dcn template static verification (build, test, compose topology)
dcn-ci:
	cd templates/dcn && go build ./...
	cd templates/dcn && go test ./...
	cd templates/dcn && $(MAKE) topology-test

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
