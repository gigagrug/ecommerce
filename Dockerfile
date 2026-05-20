FROM golang:1.26.3-alpine AS builder
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -ldflags="-s -w" -o /bin/server

FROM gcr.io/distroless/static:nonroot
WORKDIR /app
COPY --from=builder /bin/server /app/server
EXPOSE 80
USER nonroot:nonroot
ENTRYPOINT ["/app/server"]
