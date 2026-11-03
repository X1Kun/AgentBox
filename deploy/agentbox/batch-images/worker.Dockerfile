ARG PYTHON_IMAGE
FROM ${PYTHON_IMAGE}
COPY wheels/ /wheels/
RUN python3 -m pip install --no-index --no-deps /wheels/*.whl && rm -rf /wheels
COPY images/ /opt/images/
COPY images.json /opt/images.json
COPY worker.py /opt/worker.py
ENV OMP_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1 MKL_NUM_THREADS=1
USER 1000:1000
ENTRYPOINT ["python3", "-I", "-B", "-u", "/opt/worker.py"]
