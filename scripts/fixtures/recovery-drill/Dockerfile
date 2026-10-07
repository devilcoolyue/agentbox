# Disposable Linux recovery runner. No project data is built into this image.
FROM ubuntu:22.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends systemd python3 ca-certificates && apt-get clean && rm -rf /var/lib/apt/lists/*
ENV container=docker
STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd"]
