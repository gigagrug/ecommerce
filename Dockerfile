FROM golang:1.26-trixie AS builder
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -ldflags="-s -w" -o /bin/server

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata \
    && addgroup -S nonroot \
    && adduser -S nonroot -G nonroot
WORKDIR /app
COPY --from=builder /bin/server /app/server
RUN chown nonroot:nonroot /app/server
EXPOSE 80
USER nonroot:nonroot
ENTRYPOINT ["/app/server"]
