.DEFAULT_GOAL := image

WEBHOOK_SERVER ?= yangl/pod-termination-webhook:latest
HEALTH_PROXY ?= yangl/healthproxy:latest

webhook-server: $(shell find ./webhook-server -name '*.go')
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o $@ ./webhook-server

health-proxy: $(shell find ./health-proxy -name '*.go')
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o $@ ./health-proxy

.PHONY: docker-image
image: webhook-server health-proxy
	docker build -t $(WEBHOOK_SERVER) webhook-server/
	docker build -t $(HEALTH_PROXY) health-proxy/

.PHONY: push-image
push: docker-image
	docker push $(WEBHOOK_SERVER)
	docker push $(HEALTH_PROXY)

.PHONY: install
install:
	kubectl apply -f ./deployment/deployment.yaml

.PHONY: cert-manager
cert-manager:
	kubectl apply -f https://github.com/jetstack/cert-manager/releases/download/v1.3.0/cert-manager.yaml