# dcrbazaar - the dcr402 discovery index + non-custodial verify service.
#
# Build from the repository ROOT (the build needs the sibling `lib/` module that
# bazaar/go.mod resolves via `replace ../lib`):
#
#   docker build -f bazaar/Dockerfile -t dcrbazaar:latest .
#
# or just use bazaar/docker-compose.yml, which sets the context for you.

FROM golang:1.26-alpine AS build
WORKDIR /src
# Copy the two modules preserving the sibling layout so `replace ../lib` resolves.
COPY lib/ ./lib/
COPY bazaar/ ./bazaar/
WORKDIR /src/bazaar
# Pure-Go sqlite (modernc.org/sqlite) => a fully static binary, no cgo/toolchain.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/dcrbazaar ./cmd/dcrbazaar

FROM alpine:3.20
# ca-certificates: the onchain method reaches dcrdata over HTTPS.
RUN apk add --no-cache ca-certificates wget \
    && adduser -D -u 10001 bazaar \
    && mkdir -p /var/lib/dcrbazaar && chown bazaar /var/lib/dcrbazaar
COPY --from=build /out/dcrbazaar /usr/local/bin/dcrbazaar
USER bazaar
EXPOSE 8444
HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
    CMD wget -q --spider http://localhost:8444/supported || exit 1
ENTRYPOINT ["/usr/local/bin/dcrbazaar", "-config", "/etc/dcrbazaar/dcrbazaar.yaml"]
