FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
ARG TARGETOS
ARG TARGETARCH
LABEL org.opencontainers.image.title="NetBird Operator" \
      org.opencontainers.image.description="Kubernetes operator for NetBird" \
      org.opencontainers.image.source="https://github.com/ccbash/netbird-operator" \
      org.opencontainers.image.vendor="NetBird" \
      org.opencontainers.image.licenses="BSD-3-Clause"
COPY bin/${TARGETOS}-${TARGETARCH}/netbird-operator /usr/local/bin/
USER 65532:65532
ENTRYPOINT ["netbird-operator"]
