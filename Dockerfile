# Rustle as a hosted MCP server: the binary, ffmpeg and wireproxy, serving MCP over HTTP on
# $PORT. It refuses to start without RUSTLE_TOKEN; see "Hosting" in the README.
# Dockerfile.vercel is the same image for Vercel: change both together.
FROM golang:1-alpine AS build
WORKDIR /src
# With wireproxy installed, rustle-mcp http tunnels its requests through WireGuard
# (tunnel.go). Built from source, so Go's checksum database vouches for it.
RUN CGO_ENABLED=0 GOBIN=/out go install -trimpath -ldflags "-s -w" github.com/windtf/wireproxy/cmd/wireproxy@v1.1.3
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/rustle-mcp .

FROM alpine:3.24
# Alpine 3.24's ffmpeg is 8.1; the pre-started decoders need 7 or newer.
RUN apk add --no-cache ffmpeg
COPY --from=build /out/ /usr/local/bin/
# Free tiers give a fraction of a CPU, about 512 MB of memory and a small disk.
ENV RUSTLE_CACHE=/tmp/rustle-mcp RUSTLE_CACHE_MB=512 RUSTLE_WORKERS=2
USER nobody
EXPOSE 8080
CMD ["rustle-mcp", "http"]
