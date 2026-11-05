# Build stage pinned by digest; bump tag and digest together.
FROM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
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
