# --- build stage ---
FROM golang:1.22-alpine AS builder
RUN apk add --no-cache build-base sqlite-dev
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/gpkg-reverse ./main.go

# --- run stage ---
FROM alpine:3.20
COPY data/gadm_410.gpkg /data/gadm_410.gpkg
RUN apk add --no-cache ca-certificates sqlite-libs
WORKDIR /app

COPY --from=builder /out/gpkg-reverse /app/gpkg-reverse
# 运行时通过卷挂载 GPKG
ENV ADDR=:8080
ENV GPKG_PATH=/data/gadm_410.gpkg
ENV GPKG_TABLE=gadm_410
ENV GPKG_GEOM_COL=geom
ENV ROUND_PLACES=4
EXPOSE 8080
CMD ["/app/gpkg-reverse"]
