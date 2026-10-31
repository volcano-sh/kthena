FROM golang:1.26.4 AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
COPY test/e2e/tokenizer-engine-mock/ test/e2e/tokenizer-engine-mock/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /tokenizer-engine-mock ./test/e2e/tokenizer-engine-mock

FROM scratch

COPY --from=builder /tokenizer-engine-mock /tokenizer-engine-mock

USER 65532:65532
EXPOSE 8000

ENTRYPOINT ["/tokenizer-engine-mock"]
