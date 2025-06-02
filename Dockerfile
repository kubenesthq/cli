FROM debian:stable-slim

ARG KUBENEST_VERSION=latest
ARG REPO_OWNER=kubenesthq
ARG REPO_NAME=cli

WORKDIR /usr/local/bin

RUN apt-get update && apt-get install -y curl && \
    if [ "$KUBENEST_VERSION" = "latest" ]; then \
      KUBENEST_VERSION=$(curl -s https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/latest | grep tag_name | cut -d '"' -f4); \
    fi && \
    curl -L -o kubenest "https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${KUBENEST_VERSION}/kubenest-${KUBENEST_VERSION}-linux-amd64" && \
    chmod +x kubenest && \
    apt-get clean && rm -rf /var/lib/apt/lists/*
