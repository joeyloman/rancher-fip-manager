FROM docker.io/golang:1.25-alpine3.22 AS builder
RUN mkdir /src /deps
RUN apk update && apk add git build-base binutils-gold
WORKDIR /deps
ADD go.mod /deps
RUN go mod download
ADD / /src
WORKDIR /src
RUN go build -a -o rancher-fip-manager cmd/manager/main.go
FROM docker.io/alpine:3.22
RUN adduser -S -D -H -h /app rancher-fip-manager
USER rancher-fip-manager
COPY --from=builder /src/rancher-fip-manager /app/
WORKDIR /app
ENTRYPOINT ["./rancher-fip-manager"]