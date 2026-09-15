# ---- build -----------------------------------------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/correctarr ./cmd/correctarr

# ---- runtime ---------------------------------------------------------------
FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates tzdata
COPY --from=build /out/correctarr /usr/local/bin/correctarr
ENV CORRECTARR_LISTEN=:8585 \
    CORRECTARR_DATA=/config
VOLUME /config
EXPOSE 8585
ENTRYPOINT ["/usr/local/bin/correctarr"]
