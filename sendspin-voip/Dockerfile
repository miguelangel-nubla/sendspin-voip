ARG BUILD_FROM
FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG BUILD_VERSION=1.0.0

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.Version=${BUILD_VERSION}" \
    -o /bin/sendspin-voip ./cmd/sendspin-voip

FROM ${BUILD_FROM}

COPY --from=builder /bin/sendspin-voip /usr/bin/sendspin-voip
COPY sendspin-voip/run.sh /run.sh
RUN chmod +x /run.sh

CMD [ "/run.sh" ]
