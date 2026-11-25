# syntax=docker/dockerfile:1.7-labs

FROM node:20-bookworm-slim AS frontend
WORKDIR /src
# Keep the installed dependency layer reusable across application/UI changes.
COPY client/package.json client/package-lock.json client/.npmrc ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY client/ .
RUN npm run build

FROM golang:1.24-bookworm AS builder
WORKDIR /app
# Modules must live in this layer, not only a cache mount: remote cache exporters
# preserve layers, but do not export cache-mount contents between hosted runners.
COPY go.mod go.sum ./
RUN go mod download
# Frontend, documentation and release changes do not invalidate the Go build.
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY pkg/ ./pkg/
COPY neko.go ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o bin/neko_rooms cmd/neko_rooms/main.go

FROM scratch
COPY --from=builder /app/bin/neko_rooms /app/bin/neko_rooms
COPY --from=frontend /src/dist/ /var/www

ENV NEKO_ROOMS_BIND=:8080
ENV NEKO_ROOMS_ADMIN_STATIC=/var/www

EXPOSE 8080
ENTRYPOINT [ "/app/bin/neko_rooms" ]
CMD [ "serve" ]
