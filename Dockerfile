FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o mcp-server-gtasks .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/mcp-server-gtasks /usr/local/bin/mcp-server-gtasks
EXPOSE 8080
ENTRYPOINT ["mcp-server-gtasks"]
