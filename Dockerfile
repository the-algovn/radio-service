# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/radio-lab ./cmd/radio-lab \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/radio-lab-migrate ./cmd/radio-lab-migrate

FROM debian:bookworm-slim AS runtime
# ffmpeg from apt; yt-dlp as the standalone binary (a python zipapp) so it stays
# current with YouTube — the apt yt-dlp lags and breaks. python3 runs it.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg python3 ca-certificates curl \
 && curl -fsSL https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -o /usr/local/bin/yt-dlp \
 && chmod +x /usr/local/bin/yt-dlp \
 && apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*
RUN useradd -u 10001 -m app
WORKDIR /app
COPY --from=build /out/radio-lab /out/radio-lab-migrate /usr/local/bin/
COPY persona/ ./persona/
# LAB_DATA_DIR is an emptyDir mount in prod; ensure the non-root user owns /app.
RUN mkdir -p /app/lab-data && chown -R app:app /app
USER app
ENV PERSONA_DIR=/app/persona LAB_DATA_DIR=/app/lab-data
ENTRYPOINT ["radio-lab"]
