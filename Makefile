platforms := linux/arm64,linux/amd64
repo := ghcr.io/cosmonic/netreap
VERSION ?= $(shell git rev-parse --short HEAD)

.PHONY: docker ci
docker:
	docker buildx build --platform linux/arm64 --tag $(repo):$(VERSION) --load .

ci:
	docker buildx build --platform $(platforms) --tag $(repo):$(VERSION) --push .

test:
	go test -v ./...