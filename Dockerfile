# syntax=docker/dockerfile:1.7
#
# The Telegram bot. The contracts are compiled into the binary (internal/
# contracts embeds the artifacts), and the deployment record is public, so
# both are in the image; the operator's keys are not, and are mounted at run
# time from secrets/ (docker-compose.yml, beside this file). Every setting
# comes from .env, so the command is just "bot".

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
# modernc.org/sqlite is pure Go, so the binary is static and the image needs no
# libc.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/zvm-bullet ./cmd/zvm-bullet

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /out/zvm-bullet /usr/local/bin/zvm-bullet
# Who is connected to which wallet, and every game and turn: /leaderboard's
# history. The chain holds the money, so losing this loses no one's BULLET.
VOLUME /data
ENTRYPOINT ["zvm-bullet"]
CMD ["bot"]
