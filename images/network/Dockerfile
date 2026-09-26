# Build the Linux agentbox binary locally and pass its directory as context.
# Only this isolated sidecar receives NET_ADMIN; it has no workspace mounts.
FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN apk add --no-cache iptables ca-certificates
COPY agentbox /usr/local/bin/agentbox
ENTRYPOINT ["/usr/local/bin/agentbox", "network-helper"]
