# syntax=docker/dockerfile:1
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/incidentd ./cmd/incidentd

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/incidentd /app/incidentd
COPY remediation/catalog /app/remediation/catalog
COPY config/example.yaml /app/config/example.yaml
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/app/incidentd"]
CMD ["serve", "-config", "/etc/incidentd/config.yaml"]
