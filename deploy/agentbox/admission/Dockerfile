ARG BASE_IMAGE
FROM ${BASE_IMAGE}
COPY --chmod=0555 agentbox-admission /usr/local/bin/agentbox-admission
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/agentbox-admission"]
