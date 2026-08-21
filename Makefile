BINARY_NAME := sendspin-voip
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildDate=$(BUILD_DATE)

.PHONY: all build clean test test-race vet fmt fmt-check check run docker docker-multiarch

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY_NAME) ./cmd/sendspin-voip

test:
	go test -v ./...

# The race detector is implemented in C and needs cgo, so CGO_ENABLED must stay
# on here. Only the shipped binary is built with CGO_ENABLED=0.
test-race:
	CGO_ENABLED=1 go test -v -race ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

fmt-check:
	@out="$$(gofmt -s -l .)"; \
	if [ -n "$$out" ]; then \
		echo "The following files are not gofmt'd:"; echo "$$out"; exit 1; \
	fi

check: fmt-check vet test-race

run: build
	./bin/$(BINARY_NAME) -config config.example.yaml

clean:
	rm -rf bin/ dist/

docker:
	docker build -t sendspin-voip:local .

docker-multiarch:
	docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 -t ghcr.io/miguelangel-nubla/sendspin-voip:$(VERSION) .
