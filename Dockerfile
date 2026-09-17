# syntax=docker/dockerfile:1

# A versao aqui deve permanecer igual a declarada em go.mod.
FROM golang:1.27-alpine AS build

WORKDIR /src

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/app ./cmd/app


FROM alpine:3.20 AS runtime

# wget atende o healthcheck do compose; ca-certificates e tzdata sao necessarios
# para TLS e timestamps UTC corretos.
RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 app

COPY --from=build /out/app /usr/local/bin/app

USER app
EXPOSE 8080

# SIGTERM chega direto no processo (sem shell intermediario), o que e requisito
# do shutdown gracioso: parar de buscar trabalho e drenar o que esta em curso.
ENTRYPOINT ["/usr/local/bin/app"]
