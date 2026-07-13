FROM gcr.io/distroless/static:nonroot@sha256:d29e660cc75a5b6b1334e03c5c81ccf9bc0884a002c6000dbf0fb96034814478
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
