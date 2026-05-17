# Build from repository root:
#   docker build -f benchmark/kthena-router/mock-backend/Dockerfile -t kthena-mock-inference:latest .
#
# Copyright The Volcano Authors.
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /mock-inference ./benchmark/kthena-router/mock-backend

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /mock-inference /mock-inference
USER nonroot:nonroot
EXPOSE 8000
ENTRYPOINT ["/mock-inference"]
CMD ["-listen", ":8000"]
