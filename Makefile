BINARY_NAME := sendspin-voip
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildDate=$(BUILD_DATE)

# Every target the release workflow and the add-on image ship, as
# GOOS/GOARCH/GOARM triples. `make cross` proves they all still compile with
# cgo disabled — the property that makes cross-compilation possible at all.
CROSS_TARGETS := \
	linux/amd64/ \
	linux/arm64/ \
	linux/arm/7 \
	linux/386/ \
	darwin/arm64/ \
	darwin/amd64/ \
	windows/amd64/

.PHONY: all build cross clean test test-race vet fmt fmt-check check run docker docker-multiarch

all: build

# CGO_ENABLED=0 keeps the binary static and cross-compilable. Every path that
# produces a shipped artifact (this target, both Dockerfiles, release.yml)
# must keep it that way.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY_NAME) ./cmd/sendspin-voip

# Verify the release matrix still cross-compiles. Cheap: output is discarded.
cross:
	@set -e; for t in $(CROSS_TARGETS); do \
		os=$${t%%/*}; rest=$${t#*/}; arch=$${rest%%/*}; arm=$${rest#*/}; \
		printf '  %-16s' "$$os/$$arch$${arm:+v$$arm}"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch GOARM=$$arm \
			go build -trimpath -o /dev/null ./cmd/sendspin-voip && echo OK; \
	done

# Tests run under the same build constraints as the shipped binary, so what CI
# proves is what ships.
test:
	CGO_ENABLED=0 go test -v ./...

# The race detector is implemented in C, so it requires cgo — `CGO_ENABLED=0
# go test -race` fails outright with "-race requires cgo". This runs natively
# on the host and never cross-compiles, so it does not weaken the
# CGO_ENABLED=0 guarantee for anything that gets released.
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

check: fmt-check vet test test-race cross

run: build
	./bin/$(BINARY_NAME) -config config.example.yaml

clean:
	rm -rf bin/ dist/

docker:
	docker build -t sendspin-voip:local .

docker-multiarch:
	docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 -t ghcr.io/miguelangel-nubla/sendspin-voip:$(VERSION) .
