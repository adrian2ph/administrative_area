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

	List []ChildrenItem `json:"list,omitempty"`
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

// 行政区域的坐标点
type LatlngItem struct {
	GID        string `json:"code"`
	Latitude float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Name       string `json:"name"`
	ParentCode string `json:"parentCode"`
	Level      string `json:"level"`
	Elevation  float64 `json:"elevation"`
}

type LatlngRes struct {
	Code int               `json:"code"`
	Msg  string            `json:"msg"`
	Data *LatlngItem 	`json:"data"`
}


type Server struct {
	db           *sql.DB
	elevationDB  *sql.DB
	table        string
	geomCol      string
	rtreeTable   string
	sqlCandidate string
	roundPlaces  int
	googleAPIKey string
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

// 行政区层级映射
func levelNameMap() map[int]string {
	return map[int]string{
		0: "LEVEL_UNSPECIFIED",
		1: "PROVINCE",
		2: "CITY",
		3: "DISTRICT",
		4: "VILLAGE",
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
			levelName := levelNameMap()
			// GID 和 Name 成对存起来
			gids := []struct {
				gid  string
				name string
			}{
				{g0, n0},
				{g1, n1},
				{g2, n2},
				{g3, n3},
				{g4, n4},
				{g5, n5},
			}

			// 构造 ChildrenItem 列表
			list := make([]ChildrenItem, 0, 6)
			parent := ""
			for i, item := range gids {
				if item.gid != "" {
					list = append(list, ChildrenItem{
						GID:        item.gid,
						Name:       item.name,
						ParentCode: parent,
						Level:      levelName[i],
					})
					parent = item.gid
				}
			}
			return &AdminLevels{
				GID0: g0, GID1: g1, GID2: g2, GID3: g3, GID4: g4, GID5: g5,
				Name0: n0, Name1: n1, Name2: n2, Name3: n3, Name4: n4, Name5: n5,
				List: list,
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

	levelName := levelNameMap()

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

/************* 获取行政区域的中心坐标 *************/
func (s *Server) latlngOf(GID string) (*LatlngItem, error) {
	GID = strings.TrimSpace(GID)
	if GID == "" {
		return nil, fmt.Errorf("gid required")
	}

	levelName := map[int]string{
		0: "LEVEL_UNSPECIFIED",
		1: "PROVINCE",
		2: "CITY",
		3: "DISTRICT",
		4: "VILLAGE",
		5: "SUBVILLAGE",
	}

	level, err := s.detectLevel(GID)
	if err != nil {
		return nil, err
	}

	gidCol := fmt.Sprintf("GID_%d", level)
	nameCol := fmt.Sprintf("NAME_%d", level)
	var parentGidCol string
	if level > 0 {
		parentGidCol = fmt.Sprintf("GID_%d", level-1)
	} else {
		parentGidCol = "NULL"
	}

	sqlStr := fmt.Sprintf(`SELECT %s, %s, %s, %s FROM %s WHERE %s = ? LIMIT 1`,
		gidCol, nameCol, parentGidCol, s.geomCol, s.table, gidCol)

	var (
		gid       string
		name      string
		parentGid sql.NullString
		blob      []byte
	)

	err = s.db.QueryRow(sqlStr, GID).Scan(&gid, &name, &parentGid, &blob)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("gid not found")
		}
		return nil, err
	}

	wkbBytes, _, err := gpkgToWKB(blob)
	if err != nil {
		return nil, fmt.Errorf("failed to convert gpkg to wkb: %w", err)
	}

	mp, err := decodeMultiPolygon(wkbBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to decode multipolygon: %w", err)
	}

	centroid, _ := planar.CentroidArea(mp)

	return &LatlngItem{
		GID:        gid,
		Latitude:   centroid.Lat(),
		Longitude:  centroid.Lon(),
		Name:       name,
		ParentCode: parentGid.String,
		Level:      levelName[level],
		Elevation: 0.0,
	}, nil
}

type ElevationResponse struct {
	Results []struct {
		Elevation float64 `json:"elevation"`
	} `json:"results"`
	Status       string `json:"status"`
	ErrorMessage string `json:"error_message,omitempty"`
}

func (s *Server) getElevation(gid string) (float64, error) {
	var elevation float64
	err := s.elevationDB.QueryRow("SELECT elevation FROM elevations WHERE gid = ?", gid).Scan(&elevation)
	return elevation, err
}

func (s *Server) saveElevation(gid string, elevation float64) error {
	_, err := s.elevationDB.Exec("INSERT INTO elevations (gid, elevation) VALUES (?, ?)", gid, elevation)
	return err
}

func (s *Server) fetchElevationFromGoogle(lat, lon float64) (float64, error) {
	if s.googleAPIKey == "" {
		return 0, fmt.Errorf("GOOGLE_API_KEY is not set")
	}

	url := fmt.Sprintf("https://maps.googleapis.com/maps/api/elevation/json?locations=%f,%f&key=%s", lat, lon, s.googleAPIKey)
	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("google api request failed with status: %s", resp.Status)
	}

	var elevationResp ElevationResponse
	if err := json.NewDecoder(resp.Body).Decode(&elevationResp); err != nil {
		return 0, err
	}

	if elevationResp.Status != "OK" {
		return 0, fmt.Errorf("google api error: %s, message: %s", elevationResp.Status, elevationResp.ErrorMessage)
	}

	if len(elevationResp.Results) == 0 {
		return 0, fmt.Errorf("no elevation results from google api")
	}

	return elevationResp.Results[0].Elevation, nil
}


// 获取行政区域的坐标点
func (s *Server) handleLatlng(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		code = env("GPKG_PARENT_CODE", "IDN")
	}
	item, err := s.latlngOf(code)
	if err != nil {
		if strings.Contains(err.Error(), "gid not found") {
			writeErrorJSON(w, http.StatusNotFound, 404, "not found")
			return
		}
		log.Println("latlngOf error:", err)
		writeErrorJSON(w, http.StatusInternalServerError, 500, "internal error")
		return
	}

	elevation, err := s.getElevation(item.GID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			newElevation, fetchErr := s.fetchElevationFromGoogle(item.Latitude, item.Longitude)
			if fetchErr != nil {
				log.Printf("Failed to fetch elevation for GID %s: %v", item.GID, fetchErr)
				item.Elevation = 0.0
			} else {
				item.Elevation = newElevation
				log.Printf("fetch elevation for GID %s: %f", item.GID, newElevation)
				if saveErr := s.saveElevation(item.GID, newElevation); saveErr != nil {
					log.Printf("Failed to save elevation for GID %s: %v", item.GID, saveErr)
				}
			}
		} else {
			log.Printf("Failed to get elevation from cache for GID %s: %v", item.GID, err)
			item.Elevation = 0.0
		}
	} else {
		item.Elevation = elevation
	}

	w.Header().Set("Cache-Control", "public, max-age=2592000, stale-if-error=2592000")
	writeJSON(w, http.StatusOK, LatlngRes{
		Code: 200,
		Msg:  "success",
		Data: item,
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

	elevationDbPath := env("ELEVATION_DB_PATH", "data/elevations.db")
	elevationDB, err := sql.Open("sqlite3", elevationDbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open elevation db: %w", err)
	}

	_, err = elevationDB.Exec(`CREATE TABLE IF NOT EXISTS elevations (
        gid TEXT PRIMARY KEY,
        elevation REAL NOT NULL
    );`)
	if err != nil {
		return nil, fmt.Errorf("failed to create elevations table: %w", err)
	}

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
		elevationDB:  elevationDB,
		table:        table,
		geomCol:      geomCol,
		rtreeTable:   rtree,
		sqlCandidate: sqlCand,
		roundPlaces:  rp,
		googleAPIKey: env("GOOGLE_API_KEY", ""),
	}, nil
}

func main() {
	s, err := newServer()
	if err != nil {
		log.Fatal("init error:", err)
	}
	defer s.db.Close()
	defer s.elevationDB.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/reverse", s.handleReverse)
	mux.HandleFunc("/children", s.handleChildren)
	mux.HandleFunc("/latlng", s.handleLatlng)
	addr := env("ADDR", "0.0.0.0:8082")
	log.Println("http://" + addr + "/health")
	log.Println("http://" + addr + "/reverse?latitude=-6.193835958650485&longitude=106.79943779288192")
	log.Println("http://" + addr + "/children?parent_code=IDN.8_1")
	log.Println("http://" + addr + "/latlng?code=IDN.8_1")
	log.Fatal(http.ListenAndServe(addr, mux))
}