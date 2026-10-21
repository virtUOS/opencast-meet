FROM golang:1.27-alpine AS builder
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -o opencast-meet .

FROM alpine:3.24
RUN adduser -D -H -u 1000 opencast-meet
WORKDIR /app
COPY --from=builder /app/opencast-meet .
ENV APP_LISTEN_ADDR=:8080
EXPOSE 8080
USER opencast-meet
CMD ["./opencast-meet"]
