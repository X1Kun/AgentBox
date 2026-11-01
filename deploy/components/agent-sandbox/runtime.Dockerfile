ARG PYTHON_IMAGE
FROM ${PYTHON_IMAGE}
COPY sandboxd /usr/local/bin/sandboxd
RUN mkdir -p /workspace && chown 1000:1000 /workspace
WORKDIR /workspace
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/sandboxd", "--root-dir=/workspace", "--shutdown-timeout=2s"]
