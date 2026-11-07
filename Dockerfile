# syntax=docker/dockerfile:1
# Multi-stage build. In a restricted environment point the images at your mirror:
#   docker build --build-arg GO_IMAGE=mirror.local/golang:1.24 --build-arg BASE_IMAGE=mirror.local/distroless/static:nonroot .
ARG GO_IMAGE=golang:1.24
ARG BASE_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ARG VERSION=dev
# No third-party modules: nothing is downloaded during the build.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/companion ./cmd/companion \
 && mkdir -p /out/data

FROM ${BASE_IMAGE}
COPY --from=build /out/companion /companion
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATA_DIR=/data
VOLUME ["/data"]
USER 65532:65532
HEALTHCHECK --interval=60s --timeout=5s --start-period=30s --retries=3 CMD ["/companion", "healthcheck"]
ENTRYPOINT ["/companion"]
