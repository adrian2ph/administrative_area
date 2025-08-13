# --- build stage ---
FROM golang:1.22-alpine AS builder
RUN apk add --no-cache build-base sqlite-dev
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# 开启 CGO，静态/半静态构建
ENV CGO_ENABLED=1
RUN go build -o /out/gpkg-reverse ./main.go

# --- run stage ---
FROM alpine:3.20
RUN apk add --no-cache ca-certificates sqlite-libs
WORKDIR /app
COPY --from=builder /out/gpkg-reverse /app/gpkg-reverse
COPY data/gadm_410.gpkg /data/gadm_410.gpkg
COPY main.go /app/main.go
# 运行时通过卷挂载 GPKG
ENV ADDR=:8080
ENV GPKG_PATH=/data/gadm_410.gpkg
ENV GPKG_TABLE=gadm_410
ENV GPKG_GEOM_COL=geom
ENV ROUND_PLACES=4
EXPOSE 8080
CMD ["/app/gpkg-reverse"]
