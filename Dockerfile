# Backend (API) only. The screens are in ../frontend (see its own Dockerfile).
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /tablebill-backend .

FROM gcr.io/distroless/static
COPY --from=build /tablebill-backend /tablebill-backend
VOLUME /data
ENV DB_PATH=/data/tablebill.db DEMO=0 TRUST_PROXY=1
EXPOSE 8080
ENTRYPOINT ["/tablebill-backend"]
