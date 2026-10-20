# syntax=docker/dockerfile:1.7
#
# The page at /bullet/ on a ZVM gateway. Built from the repository root, not
# from web/, because the page imports the deployment record in deployments/.
# The devnet's gateway forwards /bullet/* here unchanged, so the build keeps its
# /bullet/ base and the files sit under /srv/bullet.

FROM node:22-alpine AS build
WORKDIR /src
COPY deployments ./deployments
COPY web/package.json web/package-lock.json ./web/
RUN --mount=type=cache,target=/root/.npm cd web && npm ci
COPY web ./web
# The bot the page links to. It is the only VITE_ value, and the only one Vite
# would put in the bundle, which is why it arrives as a build argument rather
# than with the rest of .env.
ARG VITE_BULLET_BOT=
# Where the explorer and Glyphswap are; empty links them under this page's own
# origin, which is right when it is served by the gateway they are on.
ARG VITE_GATEWAY=
ENV VITE_BULLET_BOT=$VITE_BULLET_BOT VITE_GATEWAY=$VITE_GATEWAY BASE_PATH=/bullet/
RUN cd web && npx vite build

FROM caddy:2.10-alpine
COPY web/Caddyfile /etc/caddy/Caddyfile
COPY --from=build /src/web/dist /srv/bullet
