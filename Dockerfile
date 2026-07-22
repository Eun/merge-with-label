FROM gcr.io/distroless/static-debian11
ARG BINARY
COPY $BINARY /app
COPY LICENSE /LICENSE
COPY licenses /licenses
ENTRYPOINT ["/app"]
