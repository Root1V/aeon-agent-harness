# syntax=docker/dockerfile:1
# Multi-stage build for any Go binary under go/cmd/<name>. Build arg CMD_PATH selects which one.
FROM golang:1.23-alpine AS build
ARG CMD_PATH
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/app ${CMD_PATH}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
