# Build stage pinned by digest; bump tag and digest together.
FROM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
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
