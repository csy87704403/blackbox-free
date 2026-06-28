FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/bridge .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/bridge /bridge
EXPOSE 39281
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/bridge", "--healthcheck"]
ENTRYPOINT ["/bridge"]
