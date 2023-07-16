FROM golang:1.20 as build

WORKDIR /go/src/github.com/Eun/merge-with-label
RUN --mount=type=bind,target=/go/src/github.com/Eun/merge-with-label,readwrite \
    CGO_ENABLED=0 go build -o /go/bin/server github.com/Eun/merge-with-label/cmd/server

RUN --mount=type=bind,target=/go/src/github.com/Eun/merge-with-label,readwrite \
    CGO_ENABLED=0 go build -o /go/bin/worker github.com/Eun/merge-with-label/cmd/worker

FROM gcr.io/distroless/static-debian11
COPY --from=build /go/bin/server /bin/
COPY --from=build /go/bin/worker /bin/
COPY LICENSE /LICENSE
COPY licenses /licenses
