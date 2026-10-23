# Builds the torrentai daemon image.
#
#   docker build -t torrentai:dev .
#   docker run -v torrentai:/data -p 6881:6881/tcp -p 6881:6881/udp \
#     -p 127.0.0.1:8737:8737 -e TORRENT_AI_API_TOKEN=change-me torrentai:dev
#
# Every flag can be set with a TORRENT_AI_* environment variable; see
# torrentai serve -h.

FROM golang:1.26.5 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/torrentai ./cmd/torrentai

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/torrentai /usr/local/bin/torrentai

# State (torrents, info dicts, DHT nodes, API socket) and downloads
# both live on one volume.
ENV TORRENT_AI_STATE_DIR=/data/state \
    TORRENT_AI_O=/data/downloads \
    TORRENT_AI_API_ADDR=:8737 \
    TORRENT_AI_LOG_FORMAT=json
VOLUME ["/data"]

# Peers (TCP), the DHT (UDP) and the control API.
EXPOSE 6881/tcp 6881/udp 8737/tcp

USER nonroot:nonroot
ENTRYPOINT ["torrentai"]
CMD ["serve"]
