# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/ericmustin/vern/cmd.version=${VERSION}" \
    -o /out/vern .

# CI image: the vern binary plus ci/vern-ci.sh. Build with --target ci.
FROM alpine:3.24 AS ci
RUN apk add --no-cache bash ca-certificates
COPY --from=build /out/vern /usr/local/bin/vern
COPY ci/vern-ci.sh /usr/local/bin/vern-ci.sh
WORKDIR /work

# Keep last: the default build target (deploy/compose.yaml) is this stage.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/vern /vern
WORKDIR /work
ENTRYPOINT ["/vern"]
