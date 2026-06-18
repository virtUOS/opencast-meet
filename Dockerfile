FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -o opencast-meet .

FROM alpine:3.24
WORKDIR /app
COPY --from=builder /app/opencast-meet .
ENV APP_LISTEN_ADDR=:8080
EXPOSE 8080
CMD ["./opencast-meet"]
