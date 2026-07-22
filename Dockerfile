FROM gcr.io/distroless/static-debian11
COPY server /server
COPY worker /worker
COPY LICENSE /LICENSE
COPY licenses /licenses
ENTRYPOINT ["/app"]
