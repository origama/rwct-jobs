# syntax=docker/dockerfile:1.7
FROM golang:1.24-alpine AS build
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/service ./cmd/${SERVICE}

FROM alpine:3.21
WORKDIR /app
RUN apk add --no-cache ca-certificates tzdata wget
COPY --from=build /out/service /app/service
COPY configs /app/configs
ENTRYPOINT ["/app/service"]
