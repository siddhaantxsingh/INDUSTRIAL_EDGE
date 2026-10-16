# Industrial edge gateway + PLC simulator (static Go binaries, stdlib only).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/edgegw ./cmd/edgegw && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/plcsim ./cmd/plcsim

FROM alpine:3.20
RUN adduser -D -u 10001 edge && mkdir -p /data && chown edge /data
COPY --from=build /out/edgegw /out/plcsim /usr/local/bin/
USER edge
VOLUME /data
EXPOSE 9100
HEALTHCHECK --interval=10s --timeout=3s CMD wget -qO- http://127.0.0.1:9100/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/edgegw"]
CMD ["-config", "/etc/edgegw/gateway.json"]
