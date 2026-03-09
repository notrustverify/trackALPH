FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM builder AS build-scraper
RUN CGO_ENABLED=0 go build -o /scraper ./cmd/scraper

FROM builder AS build-matcher
RUN CGO_ENABLED=0 go build -o /matcher ./cmd/matcher

FROM builder AS build-bot
RUN CGO_ENABLED=0 go build -o /bot ./cmd/bot

FROM builder AS build-webhook
RUN CGO_ENABLED=0 go build -o /webhook ./cmd/webhook

FROM alpine:3.21 AS scraper
RUN apk add --no-cache ca-certificates
COPY --from=build-scraper /scraper /usr/local/bin/scraper
ENTRYPOINT ["scraper"]

FROM alpine:3.21 AS matcher
RUN apk add --no-cache ca-certificates
COPY --from=build-matcher /matcher /usr/local/bin/matcher
ENTRYPOINT ["matcher"]

FROM alpine:3.21 AS bot
RUN apk add --no-cache ca-certificates
COPY --from=build-bot /bot /usr/local/bin/bot
ENTRYPOINT ["bot"]

FROM alpine:3.21 AS webhook
RUN apk add --no-cache ca-certificates
COPY --from=build-webhook /webhook /usr/local/bin/webhook
ENTRYPOINT ["webhook"]
