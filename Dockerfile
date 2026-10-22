# Build stage pinned by digest; bump tag and digest together.
FROM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY *.go openapi.json guide.md bextrack-logo.svg app-store-badge.svg google-play-badge.png ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=${VERSION}" -o /bexio-mcp .

# Empty image: no shell, package manager, sudo or libc. Just the static
# binary and the CA bundle for TLS to api.bexio.com, run as an unprivileged
# numeric user.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /bexio-mcp /bexio-mcp
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/bexio-mcp"]
