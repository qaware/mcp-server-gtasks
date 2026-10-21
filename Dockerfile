FROM golang:1.26.10-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o mcp-server-gtasks .

FROM alpine:3.21
RUN apk add --no-cache 'ca-certificates>=20261020-r0'
COPY --from=builder /app/mcp-server-gtasks /usr/local/bin/mcp-server-gtasks
EXPOSE 8080
ENTRYPOINT ["mcp-server-gtasks"]
