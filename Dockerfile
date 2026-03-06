FROM golang:1.23-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o exchange-proxy .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /app/exchange-proxy /usr/local/bin/
EXPOSE 8088
ENTRYPOINT ["exchange-proxy"]
CMD ["-addr", ":8088"]
