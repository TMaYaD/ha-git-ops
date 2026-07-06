ARG BUILD_FROM
FROM $BUILD_FROM

RUN apk add --no-cache git openssh-client age python3 py3-aiohttp py3-yaml

ARG BUILD_ARCH
ARG SOPS_VERSION=3.10.2
RUN case "${BUILD_ARCH}" in \
      aarch64) SOPS_ARCH=arm64 ;; \
      amd64)   SOPS_ARCH=amd64 ;; \
      *) echo "unsupported arch ${BUILD_ARCH}" && exit 1 ;; \
    esac \
 && wget -qO /usr/local/bin/sops \
      "https://github.com/getsops/sops/releases/download/v${SOPS_VERSION}/sops-v${SOPS_VERSION}.linux.${SOPS_ARCH}" \
 && chmod +x /usr/local/bin/sops

COPY rootfs /

CMD ["/usr/bin/run.sh"]
