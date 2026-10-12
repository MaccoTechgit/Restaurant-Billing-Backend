# Build: docker build -f deploy/Dockerfile -t restaurant-billing-backend .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \n && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
# an empty, writable data folder owned by the non-root user (a fresh volume mounted here inherits this ownership)
COPY --from=build --chown=65532:65532 /out/data /data
# the database lives on a volume so it survives restarts and upgrades
ENV DB_PATH=/data/tablebill.db DEMO=0 TRUST_PROXY=1
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/server", "-healthcheck"]
ENTRYPOINT ["/server"]
