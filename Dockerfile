# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Multi-arch compilation arguments provided by docker buildx
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILD_DATE}" \
    -o /bin/sendspin-voip ./cmd/sendspin-voip

# Final runtime image
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /bin/sendspin-voip /app/sendspin-voip

# Copy example config if needed
COPY config.example.yaml /app/config.example.yaml

ENV CONFIG_PATH=/app/config.yaml

ENTRYPOINT ["/app/sendspin-voip"]
CMD ["-config", "/app/config.yaml"]
