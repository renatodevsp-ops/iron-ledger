# syntax=docker/dockerfile:1
#
# Multi-stage build. The runtime image contains the binary and nothing else: no
# Go toolchain, no source, no shell conveniences. A smaller image means a
# smaller trusted base and a faster rollout.

FROM golang:1.25-alpine AS build

# Dependencies are resolved in their own layer so an edit to application source
# does not re-download the module cache on every build.
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is off so the binary is fully static and runs on a distroless base with no
# libc. The version is injected by the linker so a running pod can be matched to
# a commit.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/ironledger ./cmd/ironledger

# Run the tests as part of the image build. A test failure should stop the
# pipeline, not be discovered after deployment.
RUN go vet ./... && go test ./internal/... ./test/boundaries/...

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/ironledger /usr/local/bin/ironledger

# 8080 is the API; no other port is opened by this process.
EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/ironledger"]
