# Isolated terminal fixture: no model CLIs or credentials, no host mounts.
FROM alpine:3.22
RUN apk add --no-cache bash coreutils tmux python3 \
    && addgroup -g 1000 agent \
    && adduser -D -u 1000 -G agent agent \
    && mkdir -p /workspace/project-a /workspace/project-b /shared \
    && chown -R 1000:1000 /workspace /shared
USER 1000:1000
WORKDIR /workspace
CMD ["sleep", "infinity"]
