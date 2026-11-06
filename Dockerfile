FROM golang:1.22-alpine AS build
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /api ./cmd/api

FROM alpine:3.20
RUN adduser -D -H app && mkdir -p /data/uploads && chown -R app:app /data
USER app
WORKDIR /data
COPY --from=build /api /api
EXPOSE 8080
CMD ["/api"]
