ARG PYTHON_IMAGE
FROM ${PYTHON_IMAGE}
COPY agentbox-session-smoke /usr/local/bin/agentbox-session-smoke
WORKDIR /work
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/agentbox-session-smoke"]
