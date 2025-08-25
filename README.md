# 全球4层行政地址

从GADM下载的gpkg文件，根据经纬度获取4层行政地址。

三个接口：

http://0.0.0.0:8082/health
http://0.0.0.0:8082/reverse?latitude=-6.193835958650485&longitude=106.79943779288192
http://0.0.0.0:8082/children?parent_code=IDN.8_1
http://0.0.0.0:8082/latlng?code=IDN.8_1

## 经纬度坐标只需要保留4位小数

GADM 的坐标都是 EPSG:4326（WGS84），单位是经纬度度数：1° ≈ 111.32 km（赤道附近）

小数位精度换算（赤道附近）：

小数位	精度约	适用场景
3	~110 m	城市级定位
4	~11 m	街道级定位
5	~1.1 m	房屋级定位


## 下载数据 data/gadm_410.gpkg


下载geopackage: https://gadm.org/data.html

下载文件到data/gadm_410.gpkg，打开sqlite3给文件添加索引

```sql
CREATE INDEX IF NOT EXISTS idx_gadm410_gid0 ON gadm_410(GID_0);
CREATE INDEX IF NOT EXISTS idx_gadm410_gid1 ON gadm_410(GID_1);
CREATE INDEX IF NOT EXISTS idx_gadm410_gid2 ON gadm_410(GID_2);
CREATE INDEX IF NOT EXISTS idx_gadm410_gid3 ON gadm_410(GID_3);
CREATE INDEX IF NOT EXISTS idx_gadm410_gid4 ON gadm_410(GID_4);
CREATE INDEX IF NOT EXISTS idx_gadm410_gid5 ON gadm_410(GID_5);

CREATE INDEX IF NOT EXISTS idx_gadm410_lvl0_children ON gadm_410(GID_0, GID_1, NAME_1);
CREATE INDEX IF NOT EXISTS idx_gadm410_lvl1_children ON gadm_410(GID_1, GID_2, NAME_2);
CREATE INDEX IF NOT EXISTS idx_gadm410_lvl2_children ON gadm_410(GID_2, GID_3, NAME_3);
CREATE INDEX IF NOT EXISTS idx_gadm410_lvl3_children ON gadm_410(GID_3, GID_4, NAME_4);
CREATE INDEX IF NOT EXISTS idx_gadm410_lvl4_children ON gadm_410(GID_4, GID_5, NAME_5);

ANALYZE;
```

## 构建

docker buildx build --platform=linux/amd64  -t adrian2armstrong/administrative_area .