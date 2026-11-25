# syntax=docker/dockerfile:1
#
# One Dockerfile, one image per --target:
#
#   docker buildx build --target server  -t ooa-server .
#   docker buildx build --target fakeoak -t ooa-fakeoak .
#
# The addon UI image is built from frontend/addon/Dockerfile (it needs the dashboard build context).

ARG GO_VERSION=1.27

# Go always runs on the build host and cross-compiles. CGO is off (pure-Go SQLite), so a
# multi-arch build needs neither QEMU nor a cross C toolchain.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH

# A stage per binary, so changing one command does not rebuild the other target.
FROM build AS build-server
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ooa-server ./cmd/ooa-server

FROM build AS build-fakeoak
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w" -o /out/ooa-fakeoak ./cmd/ooa-fakeoak


# Alpine rather than distroless so the compose healthcheck has wget. /data is created owned by
# the runtime user: a fresh named volume copies that ownership, so SQLite can write without root.
FROM alpine:3.24 AS server
RUN adduser -D -H -u 10001 ooa \
 && mkdir /data && chown ooa:ooa /data
COPY --from=build-server /out/ooa-server /usr/local/bin/ooa-server
USER ooa
ENV OOA_DATA_DIR=/data
VOLUME /data
# 8080 is the public /v1 listener, 8081 the admin API that only the UI container should reach.
EXPOSE 8080 8081
ENTRYPOINT ["ooa-server"]


FROM gcr.io/distroless/static-debian12:nonroot AS fakeoak
COPY --from=build-fakeoak /out/ooa-fakeoak /usr/local/bin/ooa-fakeoak
EXPOSE 10000
ENTRYPOINT ["ooa-fakeoak"]
