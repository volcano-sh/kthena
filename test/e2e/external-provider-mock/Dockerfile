FROM golang:1.26.4 AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
COPY test/e2e/external-provider-mock/ test/e2e/external-provider-mock/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /external-provider-mock ./test/e2e/external-provider-mock

FROM scratch

COPY --from=builder /external-provider-mock /external-provider-mock

USER 65532:65532
EXPOSE 8080 8443

ENTRYPOINT ["/external-provider-mock"]
