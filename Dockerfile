FROM golang:1.24 AS builder
WORKDIR /src
COPY . .
RUN go build -o /teesql-mesh-conn ./...

FROM debian:bookworm-slim
COPY --from=builder /teesql-mesh-conn /usr/local/bin/teesql-mesh-conn
ENTRYPOINT ["/usr/local/bin/teesql-mesh-conn"]
