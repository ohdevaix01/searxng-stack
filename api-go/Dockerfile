FROM golang:1.22-alpine AS build
WORKDIR /src
COPY api-go/main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /api main.go

FROM scratch
COPY --from=build /api /api
COPY public /app/public
ENV PORT=8080 SEARX_URL=http://searxng:8080 CACHE_FILE=/data/cache.json
EXPOSE 8080
ENTRYPOINT ["/api"]
