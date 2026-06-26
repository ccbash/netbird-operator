FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
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
