FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY . .

# Support both context layouts:
# 1) project root context (cmd/, internal/, go.mod at /app)
# 2) parent folder context where project is in /app/trackalph
RUN set -eux; \
	if [ -f /app/go.mod ] && [ -d /app/cmd ] && [ -d /app/internal ]; then \
		ln -s /app /src; \
	elif [ -f /app/trackalph/go.mod ] && [ -d /app/trackalph/cmd ] && [ -d /app/trackalph/internal ]; then \
		ln -s /app/trackalph /src; \
	else \
		echo "Could not locate project root with go.mod + cmd + internal"; \
		ls -la /app; \
		exit 1; \
	fi

RUN go -C /src mod download

FROM builder AS build-scraper
RUN CGO_ENABLED=0 go -C /src build -o /scraper ./cmd/scraper

FROM builder AS build-matcher
RUN CGO_ENABLED=0 go -C /src build -o /matcher ./cmd/matcher

FROM builder AS build-bot
RUN CGO_ENABLED=0 go -C /src build -o /bot ./cmd/bot

FROM builder AS build-webhook
RUN CGO_ENABLED=0 go -C /src build -o /webhook ./cmd/webhook

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
