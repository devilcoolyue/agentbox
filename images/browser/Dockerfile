# Optional workspace image with a real desktop browser. Build the agent image first.
ARG AGENT_IMAGE=agentbox-agent:latest
FROM ${AGENT_IMAGE}
USER root
ARG TARGETARCH
ARG CHROME_VERSION=154.0.8037.57
# Google Chrome for Testing has no Linux arm64 distribution. ARM builds explicitly
# use Debian Chromium, with the same VNC/control path (reported to the UI).
RUN sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources
RUN apt-get -o Acquire::Retries=3 update && apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
      xvfb x11vnc openbox x11-utils xclip fonts-noto-cjk fonts-liberation \
      libnss3 libatk-bridge2.0-0 libdrm2 libxkbcommon0 libgbm1 libasound2 \
      libgtk-3-0 libxss1 libu2f-udev \
    && if [ "$(dpkg --print-architecture)" = amd64 ]; then \
      curl --fail --show-error --location --retry 3 \
        "https://storage.googleapis.com/chrome-for-testing-public/${CHROME_VERSION}/linux64/chrome-linux64.zip" -o /tmp/chrome.zip \
      && unzip -q /tmp/chrome.zip -d /opt \
      && ln -s /opt/chrome-linux64/chrome /usr/local/bin/agentbox-browser; \
    else apt-get -o Acquire::Retries=3 install -y --no-install-recommends chromium \
      && ln -s /usr/bin/chromium /usr/local/bin/agentbox-browser; fi \
    && rm -rf /var/lib/apt/lists/* /tmp/chrome.zip
RUN apt-get -o Acquire::Retries=3 update && apt-get -o Acquire::Retries=3 install -y --no-install-recommends wmctrl \
    && rm -rf /var/lib/apt/lists/*
COPY browser.py /opt/agentbox/browser.py
LABEL agentbox.browser="1"
# manager.go selects the narrow user-namespace seccomp profile for this image;
# Chrome's sandbox stays enabled. No VNC/CDP port is published.
