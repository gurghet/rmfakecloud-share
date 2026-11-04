# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/rmshare ./cmd/rmshare
# Pre-create the data directory owned by the runtime user (65532 = nonroot).
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/rmshare /usr/local/bin/rmshare
COPY --from=build --chown=65532:65532 /out/data /data
ENV RMSHARE_LISTEN=0.0.0.0:3004 \
    RMSHARE_DATA_DIR=/data
VOLUME /data
EXPOSE 3004
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/rmshare"]
