ARG BUILD_FROM

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY src/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ha-gitops .

FROM $BUILD_FROM

RUN apk add --no-cache git openssh-client age

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

COPY --from=build /ha-gitops /usr/local/bin/ha-gitops

CMD ["/usr/local/bin/ha-gitops"]
