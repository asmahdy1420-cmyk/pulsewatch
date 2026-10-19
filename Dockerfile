# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/pulsewatch ./cmd/pulsewatch

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pulsewatch /usr/local/bin/pulsewatch
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/pulsewatch"]
CMD ["-config", "/etc/pulsewatch/pulsewatch.yaml"]
