# syntax=docker/dockerfile:1

# Build stage. The module files are copied first so the dependency download is
# cached independently of the source, which is what keeps rebuilds fast.
FROM golang:1.27-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/migrate ./cmd/migrate

# Test stage: the unit suite needs no infrastructure, so it runs in the build.
FROM build AS test
RUN go vet ./... && go test -race ./...

# Runtime stage. A distroless base carries no shell and no package manager,
# which is the point: the image runs the binary and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

WORKDIR /app

COPY --from=build /out/api /app/api
COPY --from=build /out/worker /app/worker
COPY --from=build /out/migrate /app/migrate
COPY migrations /app/migrations

ENV MIGRATIONS_DIR=/app/migrations \
    HTTP_ADDRESS=:8080

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/app/api"]
