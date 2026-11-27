# Image URL to use all building/pushing image targets
IMG ?= versus-incident-operator:local
CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen

.PHONY: all
all: build

##@ Code generation

.PHONY: manifests
manifests: ## Generate CRD and RBAC manifests from markers.
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths=./internal/... output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: ## Generate DeepCopy methods.
	$(CONTROLLER_GEN) object paths=./api/...

##@ Development (required standard targets)

.PHONY: install
install: ## Install the CRD into the cluster.
	kubectl apply -f config/crd/bases

.PHONY: dev
dev: generate ## Run the controller locally against the current kube context.
	go run ./cmd

.PHONY: test
test: generate ## Run unit tests.
	go test ./... -count=1

.PHONY: lint
lint: ## Static analysis (go vet).
	go vet ./...

.PHONY: format
format: ## Format the code.
	go fmt ./...

.PHONY: typecheck
typecheck: ## Compile-time type check (no output binary).
	go build -o /dev/null ./...

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf bin

##@ Build

.PHONY: build
build: generate format lint ## Build the manager binary.
	go build -o bin/manager ./cmd

.PHONY: docker-build
docker-build: ## Build the manager image ($(IMG)).
	docker build -t $(IMG) .

##@ Cluster lifecycle (up/down = deploy/teardown of the operator)

.PHONY: deploy
deploy: ## Deploy CRD + RBAC + manager to the cluster.
	kubectl apply -f config/crd/bases
	kubectl apply -f config/rbac/role.yaml
	kubectl apply -f config/rbac/service_account.yaml
	kubectl apply -f config/rbac/role_binding.yaml
	kubectl apply -f config/manager/manager.yaml

.PHONY: undeploy
undeploy: ## Tear the controller down.
	-kubectl delete -f config/manager/manager.yaml
	-kubectl delete -f config/rbac/role_binding.yaml
	-kubectl delete -f config/rbac/service_account.yaml
	-kubectl delete -f config/rbac/role.yaml
	-kubectl delete -f config/crd/bases

.PHONY: docker-up
docker-up: deploy ## Bring the operator up in the cluster (alias: deploy).

.PHONY: docker-down
docker-down: undeploy ## Take the operator down from the cluster (alias: undeploy).
