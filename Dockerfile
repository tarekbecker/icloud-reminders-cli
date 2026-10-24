FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=1.1.0
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/reminders ./cmd/reminders

FROM alpine:3.23
ARG VERSION=1.1.0
LABEL org.opencontainers.image.version=$VERSION
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 reminders \
    && adduser -D -u 10001 -G reminders reminders \
    && mkdir /data \
    && chown reminders:reminders /data
COPY --from=build /out/reminders /usr/local/bin/reminders
USER reminders
ENV ICLOUD_REMINDERS_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["reminders"]
CMD ["serve", "--listen", ":8080"]
