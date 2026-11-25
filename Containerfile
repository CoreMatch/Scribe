# syntax=docker/dockerfile:1

FROM golang:1.26.4 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/scribe ./cmd/scribe
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/scribe /scribe
COPY --from=build --chown=65532:65532 /out/data /data
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/scribe"]
