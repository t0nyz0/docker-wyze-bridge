# syntax=docker/dockerfile:1
FROM python:3.12.15-slim-bookworm@sha256:54c85f3c47607a77f32adec749d3c81d1348bf25833671f512b26a9b6d778cb3 AS vendor
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
ARG TARGETARCH
COPY fetch.py /fetch.py
RUN python3 /fetch.py

FROM python:3.12.15-slim-bookworm@sha256:54c85f3c47607a77f32adec749d3c81d1348bf25833671f512b26a9b6d778cb3
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates ffmpeg tini \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /run/lake && chown 65532:65532 /run/lake
COPY --from=vendor /opt/agora /opt/agora
COPY --from=vendor /usr/local/bin/mediamtx /usr/local/bin/mediamtx
COPY receiver.py /opt/lake/receiver.py
ENV PYTHONPATH=/opt/agora LD_LIBRARY_PATH=/opt/agora/agora/agora_sdk \
    PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 LAKE_RUNTIME_DIR=/run/lake
USER 65532:65532
HEALTHCHECK --interval=15s --timeout=5s --start-period=90s \
    CMD ["python3", "/opt/lake/receiver.py", "--health"]
ENTRYPOINT ["/usr/bin/tini", "--", "python3", "/opt/lake/receiver.py"]
