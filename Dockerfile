FROM golang:1.18.0-alpine3.15 AS build

COPY . /go/src/smtp2tg

WORKDIR /go/src/smtp2tg
RUN go get
RUN go build main.go

FROM alpine:3.15
COPY --from=build /go/src/smtp2tg/main /usr/local/bin/smtp2tg
RUN apk add ca-certificates --no-cache
EXPOSE 25
VOLUME /config
LABEL org.opencontainers.image.source https://github.com/PaulAnnekov/smtp2tg

CMD ["/usr/local/bin/smtp2tg", "-c", "/config/smtp2tg.toml"]
