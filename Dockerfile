FROM golang:1.24 AS builder
WORKDIR /src
COPY . .
RUN go build -o /teesql-mesh-conn ./...

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates iproute2 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /teesql-mesh-conn /usr/local/bin/teesql-mesh-conn
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
