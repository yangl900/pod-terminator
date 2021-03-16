.DEFAULT_GOAL := docker-image

IMAGE ?= yangl/pod-termination-webhook:latest
REROUTER_IMAGE ?= yangl/rerouter:latest

image/webhook-server: $(shell find . -name '*.go')
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o $@ ./cmd/webhook-server

rerouter/rerouter: $(shell find ./rerouter -name '*.go')
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o $@ ./rerouter

.PHONY: docker-image
docker-image: image/webhook-server rerouter/rerouter
	docker build -t $(IMAGE) image/
	docker build -t $(REROUTER_IMAGE) rerouter/

.PHONY: push-image
push-image: docker-image
	docker push $(IMAGE)
