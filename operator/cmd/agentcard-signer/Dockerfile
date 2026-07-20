# `--platform=$BUILDPLATFORM` pins the builder stage to the host arch so
# Go cross-compiles to $TARGETARCH instead of running under QEMU. With
# CGO disabled (below), the resulting binary is bit-for-bit identical to
# a native build but ~9x faster for the arm64 variant.
FROM --platform=$BUILDPLATFORM docker.io/golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/agentcard-signer/ cmd/agentcard-signer/
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o agentcard-signer ./cmd/agentcard-signer/

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/agentcard-signer .
USER 65532:65532

ENTRYPOINT ["/agentcard-signer"]
