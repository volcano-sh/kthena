FROM python:3.12-slim AS downloader

RUN apt-get update && \
    apt-get install -y curl unzip rsync && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

RUN set -ex && \
    ARCH=$(uname -m) && \
    if [ "$ARCH" = "x86_64" ]; then \
      curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip"; \
    elif [ "$ARCH" = "aarch64" ]; then \
      curl "https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip" -o "awscliv2.zip"; \
    else \
      echo "Unsupported architecture: $ARCH" && exit 1; \
    fi && \
    unzip awscliv2.zip && \
    ./aws/install && \
    rm -rf awscliv2.zip aws

WORKDIR /app
COPY kthena/downloader/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY kthena/downloader/ ./kthena/downloader/
ENV PYTHONPATH="/app"
ENTRYPOINT ["python", "kthena/downloader/app.py"]

FROM python:3.12-slim AS runtime
RUN apt-get update && \
    apt-get install -y curl unzip rsync && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

RUN set -ex && \
    ARCH=$(uname -m) && \
    if [ "$ARCH" = "x86_64" ]; then \
      curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip"; \
    elif [ "$ARCH" = "aarch64" ]; then \
      curl "https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip" -o "awscliv2.zip"; \
    else \
      echo "Unsupported architecture: $ARCH" && exit 1; \
    fi && \
    unzip awscliv2.zip && \
    ./aws/install && \
    rm -rf awscliv2.zip aws

WORKDIR /app
COPY kthena/downloader/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY kthena/downloader/ ./kthena/downloader/
COPY kthena/runtime/ ./kthena/runtime/
RUN pip install --no-cache-dir -r kthena/runtime/requirements.txt
ENV PYTHONPATH="/app"
# Default to runtime, but can override with command
ENTRYPOINT ["python","kthena/runtime/app.py"]