FROM --platform=$BUILDPLATFORM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
WORKDIR /src
COPY source/go.mod source/go.sum ./
RUN go mod download
COPY source/ ./
COPY THIRD_PARTY_NOTICES LICENSE /out/legal/
RUN test -s /out/legal/THIRD_PARTY_NOTICES && test -s /out/legal/LICENSE
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$VERSION" -o /out/asb-server ./cmd/asb-server

FROM alpine:3.23.7@sha256:ff7f0c727dd796aa36787bfcd9a64bf35a6ce72a73451b3359fde849b6703d63
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.source="https://github.com/xelexis-dev/agent-session-broker" \
      org.opencontainers.image.description="Central broker for Claude and Codex sessions" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION
RUN mkdir /data && chown 65532:65532 /data
COPY --from=build /out/asb-server /usr/local/bin/asb-server
COPY --from=build /out/legal/ /usr/share/doc/asb/
USER 65532:65532
WORKDIR /data
EXPOSE 8787
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD test "$(wget -q -O - http://127.0.0.1:8787/healthz)" = ok || exit 1
ENTRYPOINT ["/usr/local/bin/asb-server"]
CMD ["--listen", "0.0.0.0:8787", "--memory-dir", "/data"]
