FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bexio-hosted-mcp .

# distroless/static ships CA certificates, needed for TLS to api.bexio.com.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /bexio-hosted-mcp /bexio-hosted-mcp
EXPOSE 8080
ENTRYPOINT ["/bexio-hosted-mcp"]
