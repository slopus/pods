# --- build stage -------------------------------------------------------------
FROM golang:1.25-alpine AS build

ARG VERSION=dev

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/podbay ./cmd/podbay \
 && go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/pods ./cmd/pods

# --- runtime stage -----------------------------------------------------------
FROM alpine:3.21

RUN addgroup -S pods && adduser -S -G pods pods

COPY --from=build /out/podbay /usr/local/bin/podbay
COPY --from=build /out/pods /usr/local/bin/pods

ENV PODBAY_DATA=/data
RUN mkdir -p /data && chown pods:pods /data
VOLUME /data

EXPOSE 7777
USER pods

ENTRYPOINT ["podbay"]
