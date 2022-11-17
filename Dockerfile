FROM docker.io/golang:latest as build
COPY . /usr/src/app
RUN CGO_ENABLED=0 go build -o /usr/src/app/pingo /usr/src/app/pingo.go

FROM scratch
COPY --from=build /usr/src/app/pingo /pingo
CMD ["/pingo"]

LABEL org.opencontainers.image.source=https://github.com/basilean/pingo
LABEL org.opencontainers.image.description="A simple application to measure network availability."
LABEL org.opencontainers.image.licenses=GPL-3.0-or-later
