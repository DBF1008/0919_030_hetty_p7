#!/bin/sh
# Manual test script for the GraphQL API hardening changes:
#   - end-to-end request_id propagation (pkg/proxy, pkg/api)
#   - GraphQL query complexity limit + concurrency cap (pkg/api)
#   - audit logging in mutation resolvers (pkg/api)
#   - safe body reading (pkg/httputil, pkg/api)
#
# Usage: ./test.sh
set -e

echo "==> go vet (affected packages)"
go vet ./pkg/httputil/... ./pkg/proxy/... ./pkg/api/... ./cmd/hetty/...

echo "==> go build (whole module)"
go build ./...

echo "==> unit tests: httputil (safe body reading)"
go test -count=1 -v ./pkg/httputil/...

echo "==> unit tests: proxy (request ID middleware)"
go test -count=1 -v ./pkg/proxy/...

echo "==> unit tests: api (GraphQL layer)"
go test -count=1 -v ./pkg/api/...

echo "==> unit tests: full suite (regression check)"
go test -count=1 ./...

echo "All tests passed."
