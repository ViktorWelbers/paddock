BINARIES := paddock-server paddock-gateway paddock
BIN_DIR  := bin

# Image naming. For k3d the registry prefix is unused (images are imported);
# to publish, set REGISTRY to your registry (e.g. ghcr.io/<you>).
TAG        ?= dev
REGISTRY   ?= registry.local
IMG        ?= paddock/paddock
AGENT_IMG  ?= paddock/agent-claude
AGENT_PI_IMG ?= paddock/agent-pi
K3D_CLUSTER ?= paddock
NAMESPACE  ?= paddock

# OpenAI-compatible upstream for the pi agent (empty = pi disabled).
# Point at your own model server, e.g. OPENAI_UPSTREAM=https://vllm.example.com
OPENAI_UPSTREAM ?=
OPENAI_MODEL    ?=
# Path to a PEM CA bundle when the upstream uses a private CA (optional).
OPENAI_CA       ?=

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

# Port 8080 maps to the cluster's ingress: the CLI finds the server on
# localhost:8080 with no port-forward, same shape as production-behind-ingress.
#
# The dev loop never touches the user's current kubectl context: cluster
# creation doesn't switch it, and every target below pins KUBECONFIG to the
# k3d cluster's own file.
K3D_ENV = KUBECONFIG=$$(k3d kubeconfig write $(K3D_CLUSTER))

k3d-up:
	@k3d cluster list $(K3D_CLUSTER) >/dev/null 2>&1 || k3d cluster create $(K3D_CLUSTER) -p "8080:80@loadbalancer" --kubeconfig-switch-context=false --wait

k3d-down:
	k3d cluster delete $(K3D_CLUSTER)

k3d-import: docker-build
	k3d image import -c $(K3D_CLUSTER) $(REGISTRY)/$(IMG):$(TAG) $(REGISTRY)/$(AGENT_IMG):$(TAG) $(REGISTRY)/$(AGENT_PI_IMG):$(TAG)

k3d-deploy:
	$(K3D_ENV) sh -c 'kubectl create namespace $(NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -'
	$(K3D_ENV) sh -c 'kubectl -n $(NAMESPACE) create secret generic paddock-anthropic \
		--from-literal=ANTHROPIC_API_KEY=$${ANTHROPIC_API_KEY:-sk-ant-fake} \
		--dry-run=client -o yaml | kubectl apply -f -'
	@if [ -n "$(OPENAI_UPSTREAM)" ] && [ -f "$(OPENAI_CA)" ]; then \
		$(K3D_ENV) sh -c 'kubectl -n $(NAMESPACE) create configmap openai-ca \
			--from-file=ca.crt=$(OPENAI_CA) \
			--dry-run=client -o yaml | kubectl apply -f -'; \
	fi
	$(K3D_ENV) helm upgrade --install paddock deploy/helm/paddock -n $(NAMESPACE) \
		--set image.repository=$(REGISTRY)/$(IMG) \
		--set image.tag=$(TAG) \
		--set agentImage=$(REGISTRY)/$(AGENT_IMG):$(TAG) \
		--set ingress.enabled=true \
		--set ingress.host="" \
		$(if $(OPENAI_UPSTREAM),\
			--set agentImagePi=$(REGISTRY)/$(AGENT_PI_IMG):$(TAG) \
			--set gateway.openai.upstream=$(OPENAI_UPSTREAM) \
			--set gateway.openai.model=$(OPENAI_MODEL) \
			$(if $(wildcard $(OPENAI_CA)),--set gateway.openai.caConfigMap=openai-ca,)) \
		--wait --timeout 3m

dev-up: k3d-up k3d-import k3d-deploy
	@echo "paddock is up: kubectl --context k3d-$(K3D_CLUSTER) -n $(NAMESPACE) get pods"

e2e:
	$(K3D_ENV) NAMESPACE=$(NAMESPACE) ./scripts/e2e-smoke.sh

# End-to-end for the pi agent against the OpenAI-compatible upstream.
e2e-pi:
	$(K3D_ENV) NAMESPACE=$(NAMESPACE) OPENAI_MODEL=$(OPENAI_MODEL) ./scripts/e2e-pi.sh
