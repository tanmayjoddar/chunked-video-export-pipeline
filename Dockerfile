# syntax=docker/dockerfile:1
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server

FROM debian:bookworm-slim
# Pinned ffmpeg 5.1.x from bookworm (decoders are a historic CVE surface; version pinned here).
RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg=7:5.1.* ca-certificates \
  && rm -rf /var/lib/apt/lists/* \
  && ffmpeg -version | head -n 1
RUN useradd -r -u 10001 appuser
RUN mkdir -p /data/media /data/tusd /tmp && chown -R appuser /data /tmp
USER appuser
COPY --from=build /out/server /usr/local/bin/server
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
