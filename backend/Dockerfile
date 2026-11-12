FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./...
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/sims4-mods .

FROM alpine:3.21
RUN addgroup -S -g 101 app && adduser -S -u 100 -G app app
COPY --from=build /out/sims4-mods /usr/local/bin/sims4-mods
RUN mkdir -p /data /archive && chown -R 100:101 /data /archive
USER 100:101
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/sims4-mods"]
