// main.go
package main

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/wkb"
	"github.com/paulmach/orb/planar"
)

type AdminLevels struct {
	GID0 string `json:"level0Code,omitempty"`
	GID1 string `json:"level1Code,omitempty"`
	GID2 string `json:"level2Code,omitempty"`
	GID3 string `json:"level3Code,omitempty"`
	GID4 string `json:"level4Code,omitempty"`
	GID5 string `json:"level5Code,omitempty"`

	Name0 string `json:"level0Name"`
	Name1 string `json:"level1Name,omitempty"`
	Name2 string `json:"level2Name,omitempty"`
	Name3 string `json:"level3Name,omitempty"`
	Name4 string `json:"level4Name,omitempty"`
	Name5 string `json:"level5Name,omitempty"`
}

type AdminLevelsRes struct {
	Code int           `json:"code"`
	Msg  string        `json:"msg"`
	Data *AdminLevels  `json:"data"`
}

type ChildrenItem struct {
	GID        string `json:"code"`
	Name       string `json:"name"`
	ParentCode string `json:"parentCode"`
	Level      string `json:"level"`
}
type ChildrenItemList struct {
	List []ChildrenItem `json:"list"`
}
type ChildrenRes struct {
	Code int               `json:"code"`
	Msg  string            `json:"msg"`
	Data *ChildrenItemList `json:"data"`
}

type Server struct {
	db           *sql.DB
	table        string
	geomCol      string
	rtreeTable   string
	sqlCandidate string
	roundPlaces  int
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

/************* 统一 JSON 响应工具（错误固定 ChildrenRes） *************/
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErrorJSON(w http.ResponseWriter, httpStatus, bizCode int, msg string) {
	// 所有错误一律用 ChildrenRes 格式返回
	resp := ChildrenRes{
		Code: bizCode,
		Msg:  msg,
		Data: nil, // 或者 &ChildrenItemList{List: []ChildrenItem{}}
	}
	writeJSON(w, httpStatus, resp)
}

/************* GeoPackage Binary -> WKB *************/
func gpkgToWKB(b []byte) ([]byte, int32, error) {
	if len(b) < 8 {
		return nil, 0, errors.New("geom too short")
	}
	if b[0] != 'G' || b[1] != 'P' {
		return b, 0, nil
	}
	flags := b[3]
	env := (flags >> 1) & 0x07
	var envCount int
	switch env {
	case 0:
		envCount = 0
	case 1:
		envCount = 4
	case 2, 3:
		envCount = 6
	case 4:
		envCount = 8
	default:
		envCount = 0
	}
	srsBE := int32(binary.BigEndian.Uint32(b[4:8]))
	srsLE := int32(binary.LittleEndian.Uint32(b[4:8]))
	srid := srsBE
	if srid < -1 || srid > 1000000 {
		srid = srsLE
	}
	offset := 8 + envCount*8
	if len(b) < offset+1 {
		return nil, 0, errors.New("invalid gpkg header/envelope")
	}
	return b[offset:], srid, nil
}

func decodeMultiPolygon(w []byte) (orb.MultiPolygon, error) {
	g, err := wkb.Unmarshal(w)
	if err != nil {
		return nil, err
	}
	switch gg := g.(type) {
	case orb.MultiPolygon:
		return gg, nil
	case orb.Polygon:
		return orb.MultiPolygon{gg}, nil
	default:
		return nil, fmt.Errorf("unsupported geometry: %T", g)
	}
}

/************* 反向地理 *************/
func (s *Server) reverse(lon, lat float64) (*AdminLevels, error) {
	f := math.Pow10(s.roundPlaces)
	rlon := math.Round(lon*f) / f
	rlat := math.Round(lat*f) / f

	rows, err := s.db.Query(s.sqlCandidate, rlon, rlon, rlat, rlat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			g0, g1, g2, g3, g4, g5 string
			n0, n1, n2, n3, n4, n5 string
			blob                   []byte
		)
		if err := rows.Scan(&g0, &g1, &g2, &g3, &g4, &g5, &n0, &n1, &n2, &n3, &n4, &n5, &blob); err != nil {
			return nil, err
		}
		wkbBytes, _, err := gpkgToWKB(blob)
		if err != nil {
			continue
		}
		mp, err := decodeMultiPolygon(wkbBytes)
		if err != nil {
			continue
		}
		if planar.MultiPolygonContains(mp, orb.Point{rlon, rlat}) {
			return &AdminLevels{
				GID0: g0, GID1: g1, GID2: g2, GID3: g3, GID4: g4, GID5: g5,
				Name0: n0, Name1: n1, Name2: n2, Name3: n3, Name4: n4, Name5: n5,
			}, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return nil, sql.ErrNoRows
}

/************* Children（父→子列表） *************/
func (s *Server) childrenOf(parentGID string) ([]ChildrenItem, error) {
	parentGID = strings.TrimSpace(parentGID)
	if parentGID == "" {
		return nil, fmt.Errorf("gid required")
	}

	levelName := map[int]string{
		0: "LEVEL_UNSPECIFIED",
		1: "PROVINCE",
		2: "CITY",
		3: "DISTRICT",
		4: "VILLAGE",
	}

	level, err := s.detectLevel(parentGID)
	if err != nil {
		return nil, err
	}
	if level == 5 {
		return []ChildrenItem{}, nil
	}

	childGIDCol := fmt.Sprintf("GID_%d", level+1)
	childNameCol := fmt.Sprintf("NAME_%d", level+1)
	parentCol := fmt.Sprintf("GID_%d", level)

	sqlStr := fmt.Sprintf(`
SELECT DISTINCT %s, %s
FROM %s
WHERE %s = ?
  AND %s IS NOT NULL
ORDER BY %s COLLATE NOCASE;`,
		childGIDCol, childNameCol, s.table, parentCol, childGIDCol, childNameCol)

	rows, err := s.db.Query(sqlStr, parentGID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChildrenItem
	for rows.Next() {
		var gid, name sql.NullString
		if err := rows.Scan(&gid, &name); err != nil {
			return nil, err
		}
		if gid.Valid && name.Valid && len(gid.String) > 0 {
			out = append(out, ChildrenItem{
				GID:        gid.String,
				Name:       name.String,
				ParentCode: parentGID,
				Level:      levelName[level+1],
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// 检测 GID 属于哪一层（0..5）
func (s *Server) detectLevel(gid string) (int, error) {
	for lvl := 0; lvl <= 5; lvl++ {
		col := fmt.Sprintf("GID_%d", lvl)
		sqlStr := fmt.Sprintf("SELECT 1 FROM %s WHERE %s = ? LIMIT 1;", s.table, col)
		var one int
		err := s.db.QueryRow(sqlStr, gid).Scan(&one)
		if err == nil {
			return lvl, nil
		}
		if !errors.Is(err, sql.ErrNoRows) && err != nil {
			return 0, err
		}
	}
	return 0, fmt.Errorf("gid not found in any level")
}

/************* HTTP 层 *************/
func parseLatLon(r *http.Request) (lat float64, lon float64, err error) {
	q := r.URL.Query()
	if ll := q.Get("latlng"); ll != "" {
		parts := strings.Split(ll, ",")
		if len(parts) != 2 {
			return 0, 0, fmt.Errorf("invalid latlng, use 'lat,lon'")
		}
		lat, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		lon, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err1 != nil || err2 != nil {
			return 0, 0, fmt.Errorf("invalid latlng values")
		}
		return lat, lon, nil
	}
	latStr := q.Get("latitude")
	lonStr := q.Get("longitude")
	if latStr == "" || lonStr == "" {
		return 0, 0, fmt.Errorf("latitude/longitude or latlng are required")
	}
	lat, err1 := strconv.ParseFloat(latStr, 64)
	lon, err2 := strconv.ParseFloat(lonStr, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("invalid latitude/longitude values")
	}
	return lat, lon, nil
}

func (s *Server) handleReverse(w http.ResponseWriter, r *http.Request) {
	lat, lon, err := parseLatLon(r)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		writeErrorJSON(w, http.StatusBadRequest, 400, "lat/lon out of range")
		return
	}
	res, err := s.reverse(lon, lat)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrorJSON(w, http.StatusNotFound, 404, "not found")
			return
		}
		log.Println("reverse error:", err)
		writeErrorJSON(w, http.StatusInternalServerError, 500, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, AdminLevelsRes{
		Code: 200,
		Msg:  "success",
		Data: res,
	})
}

func (s *Server) handleChildren(w http.ResponseWriter, r *http.Request) {
	parentCode := strings.TrimSpace(r.URL.Query().Get("parent_code"))
	if parentCode == "" {
		parentCode = env("GPKG_PARENT_CODE", "IDN")
	}
	items, err := s.childrenOf(parentCode)
	if err != nil {
		// 标准化 404 判定
		if strings.Contains(err.Error(), "not found") {
			items = make([]ChildrenItem, 0)
		} else {
			log.Println("children error:", err)
			writeErrorJSON(w, http.StatusInternalServerError, 500, "internal error")
			return
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=2592000, stale-if-error=2592000")
	writeJSON(w, http.StatusOK, ChildrenRes{
		Code: 200,
		Msg:  "success",
		Data: &ChildrenItemList{List: items},
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

/************* 启动 *************/
func newServer() (*Server, error) {
	gpkgPath := env("GPKG_PATH", "data/gadm_410.gpkg")
	table := env("GPKG_TABLE", "gadm_410")
	geomCol := env("GPKG_GEOM_COL", "geom")
	roundStr := env("ROUND_PLACES", "4")
	rp, _ := strconv.Atoi(roundStr)
	if rp < 0 || rp > 6 {
		rp = 4
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&cache=shared&_busy_timeout=5000&immutable=1", gpkgPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(5 * time.Minute)

	rtree := fmt.Sprintf("rtree_%s_%s", table, geomCol)
	sqlCand := fmt.Sprintf(`
SELECT a.GID_0, a.GID_1, a.GID_2, a.GID_3, a.GID_4, a.GID_5,
       a.NAME_0, a.NAME_1, a.NAME_2, a.NAME_3, a.NAME_4, a.NAME_5,
       a.%s
FROM %s AS a
JOIN %s AS r ON a.rowid = r.id
WHERE r.minx <= ? AND r.maxx >= ? AND r.miny <= ? AND r.maxy >= ?
LIMIT 200;`, geomCol, table, rtree)

	return &Server{
		db:           db,
		table:        table,
		geomCol:      geomCol,
		rtreeTable:   rtree,
		sqlCandidate: sqlCand,
		roundPlaces:  rp,
	}, nil
}

func main() {
	s, err := newServer()
	if err != nil {
		log.Fatal("init error:", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/reverse", s.handleReverse)
	mux.HandleFunc("/children", s.handleChildren)

	addr := env("ADDR", "0.0.0.0:8082")
	log.Println("http://" + addr + "/health")
	log.Println("http://" + addr + "/reverse?latitude=-6.193835958650485&longitude=106.79943779288192")
	log.Println("http://" + addr + "/children?parent_code=IDN.8_1")
	log.Fatal(http.ListenAndServe(addr, mux))
}
