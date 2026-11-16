FROM golang:1.27-alpine AS builder
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -o opencast-meet .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=builder /app/opencast-meet /opencast-meet
ENV APP_LISTEN_ADDR=:8080
EXPOSE 8080
CMD ["/opencast-meet"]
