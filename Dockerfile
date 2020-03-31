FROM golang:1.13.6-alpine AS build-env
COPY . /go/src/github.com/capitalonline/cds-nas-controller
RUN cd /go/src/github.com/capitalonline/cds-nas-controller/cmd && \
    CGO_ENABLED=1 go build -tags 'netgo' --ldflags '-extldflags "-static"' -o /cds-nas-controller -v

FROM alpine:3.7
RUN apk update --no-cache && apk add ca-certificates
COPY --from=build-env /cds-nas-controller /cds-nas-controller

ENTRYPOINT ["/cds-nas-controller"]