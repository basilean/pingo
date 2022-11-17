FROM docker.io/golang:latest as build
COPY . /usr/src/app
RUN CGO_ENABLED=0 go build -o /usr/src/app/pingo /usr/src/app/pingo.go

FROM scratch
COPY --from=build /usr/src/app/pingo /pingo
CMD ["/pingo"]
