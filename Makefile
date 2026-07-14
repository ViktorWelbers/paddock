BINARIES := paddock-server paddock-gateway paddock
BIN_DIR  := bin

# Image naming. For k3d the registry prefix is unused (images are imported);
# for the homelab, REGISTRY=harbor.internal.
TAG        ?= dev
REGISTRY   ?= harbor.internal
IMG        ?= paddock/paddock
AGENT_IMG  ?= paddock/agent-claude
AGENT_PI_IMG ?= paddock/agent-pi
K3D_CLUSTER ?= paddock
NAMESPACE  ?= paddock

# OpenAI-compatible upstream for the pi agent (empty = pi disabled).
OPENAI_UPSTREAM ?= https://vllm.internal
OPENAI_MODEL    ?= cyankiwi/gemma-4-26B-A4B-it-AWQ-4bit
OPENAI_CA       ?= $(HOME)/Code/infrastructure/k3s/cert-manager/k8s-home-ca.crt

.PHONY: build test vet lint clean helm-lint docker-build \
        k3d-up k3d-down k3d-import k3d-deploy dev-up e2e e2e-pi push

build:
	@mkdir -p $(BIN_DIR)
	@for b in $(BINARIES); do \
		echo "building $$b"; \
		go build -o $(BIN_DIR)/$$b ./cmd/$$b || exit 1; \
	done

test:
	go test ./...

vet:
	go vet ./...

lint: vet
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed, skipping"

helm-lint:
	helm lint deploy/helm/paddock

clean:
	rm -rf $(BIN_DIR)

## --- Images ---------------------------------------------------------------

docker-build:
	docker build -t $(REGISTRY)/$(IMG):$(TAG) .
	docker build -t $(REGISTRY)/$(AGENT_IMG):$(TAG) -f Dockerfile.agent .
	docker build -t $(REGISTRY)/$(AGENT_PI_IMG):$(TAG) -f Dockerfile.agent-pi .

push: docker-build
	docker push $(REGISTRY)/$(IMG):$(TAG)
	docker push $(REGISTRY)/$(AGENT_IMG):$(TAG)
	docker push $(REGISTRY)/$(AGENT_PI_IMG):$(TAG)

## --- k3d dev loop ----------------------------------------------------------
# make dev-up   -> cluster + images + deploy (fake key unless ANTHROPIC_API_KEY set)
# make e2e      -> end-to-end smoke test against the running deploy

k3d-up:
	@k3d cluster list $(K3D_CLUSTER) >/dev/null 2>&1 || k3d cluster create $(K3D_CLUSTER) --wait

k3d-down:
	k3d cluster delete $(K3D_CLUSTER)

k3d-import: docker-build
	k3d image import -c $(K3D_CLUSTER) $(REGISTRY)/$(IMG):$(TAG) $(REGISTRY)/$(AGENT_IMG):$(TAG) $(REGISTRY)/$(AGENT_PI_IMG):$(TAG)

k3d-deploy:
	kubectl create namespace $(NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -
	kubectl -n $(NAMESPACE) create secret generic paddock-anthropic \
		--from-literal=ANTHROPIC_API_KEY=$${ANTHROPIC_API_KEY:-sk-ant-fake} \
		--dry-run=client -o yaml | kubectl apply -f -
	@if [ -n "$(OPENAI_UPSTREAM)" ] && [ -f "$(OPENAI_CA)" ]; then \
		kubectl -n $(NAMESPACE) create configmap homelab-ca \
			--from-file=ca.crt=$(OPENAI_CA) \
			--dry-run=client -o yaml | kubectl apply -f -; \
	fi
	helm upgrade --install paddock deploy/helm/paddock -n $(NAMESPACE) \
		--set image.repository=$(REGISTRY)/$(IMG) \
		--set image.tag=$(TAG) \
		--set agentImage=$(REGISTRY)/$(AGENT_IMG):$(TAG) \
		$(if $(OPENAI_UPSTREAM),\
			--set agentImagePi=$(REGISTRY)/$(AGENT_PI_IMG):$(TAG) \
			--set gateway.openai.upstream=$(OPENAI_UPSTREAM) \
			--set gateway.openai.model=$(OPENAI_MODEL) \
			$(if $(wildcard $(OPENAI_CA)),--set gateway.openai.caConfigMap=homelab-ca,)) \
		--wait --timeout 3m

dev-up: k3d-up k3d-import k3d-deploy
	@echo "paddock is up: kubectl -n $(NAMESPACE) get pods"

e2e:
	NAMESPACE=$(NAMESPACE) ./scripts/e2e-smoke.sh

# End-to-end for the pi agent against the OpenAI-compatible upstream.
e2e-pi:
	NAMESPACE=$(NAMESPACE) OPENAI_MODEL=$(OPENAI_MODEL) ./scripts/e2e-pi.sh
