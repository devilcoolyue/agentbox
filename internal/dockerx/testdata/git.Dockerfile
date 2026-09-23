# Minimal Linux fixture: no model CLIs, credentials, or host mounts.
FROM alpine:3
RUN apk add --no-cache git coreutils \
    && addgroup -g 1000 agent \
    && adduser -D -u 1000 -G agent agent \
    && mkdir /workspace \
    && chown 1000:1000 /workspace
USER 1000:1000
WORKDIR /workspace
CMD ["sleep", "infinity"]
