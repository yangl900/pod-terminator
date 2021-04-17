.DEFAULT_GOAL := docker-image

IMAGE ?= yangl/pod-termination-webhook:latest
REROUTER_IMAGE ?= yangl/rerouter:latest
HEALTH_PROXY ?= yangl/healthproxy:latest

image/webhook-server: $(shell find . -name '*.go')
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o $@ ./cmd/webhook-server

rerouter/rerouter: $(shell find ./rerouter -name '*.go')
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o $@ ./rerouter

health-proxy/health-proxy: $(shell find ./health-proxy -name '*.go')
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o $@ ./health-proxy

.PHONY: docker-image
docker-image: image/webhook-server rerouter/rerouter health-proxy/health-proxy
	docker build -t $(IMAGE) image/
	docker build -t $(REROUTER_IMAGE) rerouter/
	docker build -t $(HEALTH_PROXY) health-proxy/

.PHONY: push-image
push: docker-image
	docker push $(IMAGE)
	docker push $(HEALTH_PROXY)
