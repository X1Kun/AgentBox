# Reuse the digest-pinned base already used by the session lab.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
COPY --chmod=0555 agentbox-api /usr/local/bin/agentbox-api
USER 1000:1000
WORKDIR /tmp
ENTRYPOINT ["/usr/local/bin/agentbox-api"]
