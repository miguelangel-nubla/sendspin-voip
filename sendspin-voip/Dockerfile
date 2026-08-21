ARG BUILD_FROM

# Pin the builder to the *build* platform. Without --platform=$BUILDPLATFORM the
# Go toolchain itself is pulled for the target architecture and executed under
# QEMU, which makes armv7/aarch64 add-on builds an order of magnitude slower and
# occasionally flaky. Go cross-compiles natively, so build on the host and
# target via GOOS/GOARCH instead. This matches the root Dockerfile.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /src

# Cache dependencies separately from the source tree.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
# BUILD_VERSION is supplied by the Home Assistant add-on builder.
ARG BUILD_VERSION=dev

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w -X main.Version=${BUILD_VERSION}" \
    -o /bin/sendspin-voip ./cmd/sendspin-voip

FROM ${BUILD_FROM}

COPY --from=builder /bin/sendspin-voip /usr/bin/sendspin-voip
COPY sendspin-voip/run.sh /run.sh
RUN chmod +x /run.sh

CMD [ "/run.sh" ]
