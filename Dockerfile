# syntax=docker/dockerfile:1
# The Quai image: one static binary with the frontend embedded, plus git and ssh for transport
# (docs/operations/deployment-and-operations.md, Packaging). From the repository root:
#   docker build --build-arg VERSION=$(git describe --always) -t quai .
# The role is a sub-command: quai server (the default), quai worker, quai migrate.

FROM node:22-alpine@sha256:fe4f6d74e20370acb04ad889077d14d0f64a9c8ea6a42bc60cddf1e844699914 AS frontend
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
# Bundling only: CI typechecks, lints and tests (the tests read files outside frontend/).
RUN npx vite build

FROM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS backend
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
COPY --from=frontend /src/frontend/dist/ ./internal/web/dist/
ARG VERSION=dev
RUN go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/quai ./cmd/quai

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
# The group is root so that OpenShift's arbitrary user ids, which run in it, can write the cache.
RUN apk add --no-cache git openssh-client openssh-keygen ca-certificates tzdata \
 && adduser -D -H -u 10001 -G root -s /sbin/nologin quai \
 && mkdir -p /var/cache/quai \
 && chown 10001:0 /var/cache/quai \
 && chmod 0770 /var/cache/quai
COPY --from=backend /out/quai /usr/local/bin/quai
ENV QUAI_GIT_CACHE_DIR=/var/cache/quai
USER 10001
EXPOSE 8080
VOLUME /var/cache/quai
ENTRYPOINT ["/usr/local/bin/quai"]
CMD ["server"]
