BINARY_NAME := sendspin-voip
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildDate=$(BUILD_DATE)

.PHONY: all build clean test lint run docker docker-multiarch

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY_NAME) ./cmd/sendspin-voip

test:
	CGO_ENABLED=0 go test -v -race ./...

run: build
	./bin/$(BINARY_NAME) -config config.example.yaml

clean:
	rm -rf bin/ dist/

docker:
	docker build -t sendspin-voip:local .

docker-multiarch:
	docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 -t ghcr.io/miguelangel-nubla/sendspin-voip:$(VERSION) .
