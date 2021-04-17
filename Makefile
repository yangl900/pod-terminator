.DEFAULT_GOAL := docker-image

IMAGE ?= yangl/pod-termination-webhook:latest
HEALTH_PROXY ?= yangl/healthproxy:latest

image/webhook-server: $(shell find . -name '*.go')
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o $@ ./cmd/webhook-server

health-proxy/health-proxy: $(shell find ./health-proxy -name '*.go')
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o $@ ./health-proxy

.PHONY: docker-image
image: image/webhook-server rerouter/rerouter health-proxy/health-proxy
	docker build -t $(IMAGE) image/
	docker build -t $(HEALTH_PROXY) health-proxy/

.PHONY: push-image
push: docker-image
	docker push $(IMAGE)
	docker push $(HEALTH_PROXY)

.PHONY: install
install:
	kubectl apply -f ./deployment/deployment.yaml

.PHONY: cert-manager
cert-manager:
	kubectl apply -f https://github.com/jetstack/cert-manager/releases/download/v1.3.0/cert-manager.yaml