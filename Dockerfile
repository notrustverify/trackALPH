FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY . .

# Discover project root anywhere under /app (up to 3 levels deep),
# requiring go.mod + cmd + internal.
RUN set -eux; \
	src=""; \
	for d in /app /app/* /app/*/* /app/*/*/*; do \
		if [ -d "$d" ] && [ -f "$d/go.mod" ] && [ -d "$d/cmd" ] && [ -d "$d/internal" ]; then \
			src="$d"; \
			break; \
		fi; \
	done; \
	if [ -z "$src" ]; then \
		echo "Could not locate project root with go.mod + cmd + internal"; \
		ls -la /app; \
		exit 1; \
	fi; \
	ln -s "$src" /src

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
