FROM golang:1.20 as build

WORKDIR /go/src/github.com/Eun/merge-with-label
RUN --mount=type=bind,target=/go/src/github.com/Eun/merge-with-label,readwrite \
    CGO_ENABLED=0 go build -o /go/bin/app github.com/Eun/merge-with-label

FROM gcr.io/distroless/static-debian11

COPY --from=build /go/bin/app /
CMD ["/app"]
