# Builds the search sidecar with the ONNX embedder compiled in.
#
# Three things are fetched here at build time, pinned to the exact
# versions the repo pins: the ONNX Runtime shared library, libtokenizers.a,
# and the model weights (see README.md "Native dependencies").
#
# Debian, not Alpine: the prebuilt ONNX Runtime and libtokenizers releases
# are glibc-linked, and musl cannot load them.

ARG ONNXRUNTIME_VERSION=1.30.0
ARG TOKENIZERS_VERSION=v1.27.0
ARG MODEL_REPO=nomic-ai/nomic-embed-text-v1.5

FROM docker.io/library/golang:1.27-trixie AS build

ARG ONNXRUNTIME_VERSION
ARG TOKENIZERS_VERSION
ARG MODEL_REPO
# Set by BuildKit: "amd64" or "arm64".
ARG TARGETARCH

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# 1. ONNX Runtime. Release tarballs name aarch64 where Go says arm64.
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) ort_arch=x64 ;; \
      arm64) ort_arch=aarch64 ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/ort.tgz \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/onnxruntime-linux-${ort_arch}-${ONNXRUNTIME_VERSION}.tgz"; \
    mkdir -p /opt/onnxruntime; \
    tar -xzf /tmp/ort.tgz -C /opt/onnxruntime --strip-components=1; \
    rm /tmp/ort.tgz; \
    test -f /opt/onnxruntime/lib/libonnxruntime.so

# 2. libtokenizers.a. Version MUST match go.mod's daulet/tokenizers
#    requirement -- the Rust ABI is not stable across releases.
RUN set -eux; \
    curl -fsSL -o /tmp/tokenizers.tgz \
      "https://github.com/daulet/tokenizers/releases/download/${TOKENIZERS_VERSION}/libtokenizers.linux-${TARGETARCH}.tar.gz"; \
    mkdir -p /opt/tokenizers; \
    tar -xzf /tmp/tokenizers.tgz -C /opt/tokenizers; \
    rm /tmp/tokenizers.tgz; \
    test -f /opt/tokenizers/libtokenizers.a

# 3. The model. hugot expects tokenizer.json and friends one directory up
#    from the .onnx file, so the layout here is load-bearing. Use the
#    int8-quantized export, not fp32 (roughly half the throughput for no
#    accuracy benefit). The directory NAME matters: the embedder derives
#    its identity from the model directory's basename, which is folded
#    into the content hash deciding which entries need re-embedding --
#    renaming it later would re-index the whole corpus.
RUN set -eux; \
    model_dir="/opt/models/$(basename "${MODEL_REPO}")"; \
    mkdir -p "${model_dir}/onnx"; \
    base="https://huggingface.co/${MODEL_REPO}/resolve/main"; \
    for f in tokenizer.json vocab.txt config.json tokenizer_config.json special_tokens_map.json; do \
      curl -fsSL -o "${model_dir}/${f}" "${base}/${f}"; \
    done; \
    curl -fsSL -o "${model_dir}/onnx/model_quantized.onnx" "${base}/onnx/model_quantized.onnx"; \
    test -s "${model_dir}/onnx/model_quantized.onnx"

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# -tags ORT selects the real ONNX backend. Without it the build silently
# succeeds against a pure-Go backend that is roughly 10x slower.
RUN CGO_ENABLED=1 CGO_LDFLAGS="-L/opt/tokenizers" \
    go build -tags ORT -o /out/sidecar ./cmd/sidecar

FROM docker.io/library/debian:trixie-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/sidecar /usr/bin/sidecar
COPY --from=build /opt/onnxruntime/lib/ /usr/local/lib/onnxruntime/
COPY --from=build /opt/models/ /opt/models/

# Defaults matching the layout above. Both are overridable; the database
# URL is not, and the sidecar refuses to start without it.
ENV SIDECAR_MODEL_PATH=/opt/models/nomic-embed-text-v1.5/onnx/model_quantized.onnx \
    SIDECAR_ONNX_LIB_DIR=/usr/local/lib/onnxruntime

# 127.0.0.1 (the default) is unreachable from outside the container; bind
# all interfaces instead. Safe here since the port is only published to
# the compose network.
ENV SIDECAR_ADMIN_ADDR=0.0.0.0:8081

EXPOSE 8081
USER 65534
CMD ["/usr/bin/sidecar"]
