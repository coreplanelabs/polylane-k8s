# Build the polylane-k8s binary: static, CGO-free, distroless runtime.
# Multi-arch: the build stage runs on the build host's platform and
# cross-compiles for $TARGETARCH, so arm64 images do not pay the QEMU tax.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:dc2521c2a906db43073b8b4d99f491b6341cf15610b6ebbab187c45153f9959e AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
ARG VERSION=
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-X github.com/coreplanelabs/polylane-k8s/internal/buildinfo.Commit=${GIT_COMMIT} -X github.com/coreplanelabs/polylane-k8s/internal/buildinfo.BuildTime=${BUILD_TIME} ${VERSION:+-X github.com/coreplanelabs/polylane-k8s/internal/buildinfo.Version=${VERSION}}" \
    -o /out/polylane-k8s ./cmd/polylane-k8s

FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b
COPY --from=build /out/polylane-k8s /polylane-k8s
EXPOSE 8081 9090
ENTRYPOINT ["/polylane-k8s"]
CMD ["run", "--config", "/etc/polylane/config.yaml"]
