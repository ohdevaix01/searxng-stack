package main

import (
"encoding/json"
"fmt"
"log"
"net/http"
"net/url"
"os"
"strconv"
"strings"
"sync"
"time"
)

type cacheEntry struct {
Body    []byte
Created time.Time
}

type Stats struct {
Requests   int64 `json:"requests"`
CacheHits  int64 `json:"cache_hits"`
Errors     int64 `json:"errors"`
}

var (
searxURL   = env("SEARX_URL", "http://searxng:8080")
port       = env("PORT", "8080")
defLimit   = envInt("DEFAULT_LIMIT", 20)
maxLimit   = envInt("MAX_LIMIT", 50)
cacheTTL   = time.Duration(envInt("CACHE_TTL", 3600)) * time.Second
apiKey     = os.Getenv("API_KEY")
upTimeout  = time.Duration(envInt("UPSTREAM_TIMEOUT", 25)) * time.Second
cachePath  = env("CACHE_FILE", "/data/cache.json")

mu    sync.RWMutex
cache = map[string]cacheEntry{}
stats Stats
dirty bool
)

func env(k, d string) string {
if v := os.Getenv(k); v != "" {
return v
}
return d
}

func envInt(k string, d int) int {
if v := os.Getenv(k); v != "" {
if n, err := strconv.Atoi(v); err == nil {
return n
}
}
return d
}

func loadCache() {
data, err := os.ReadFile(cachePath)
if err != nil {
return
}
var raw map[string]struct {
Body    []byte    `json:"b"`
Created time.Time `json:"t"`
}
if err := json.Unmarshal(data, &raw); err != nil {
return
}
now := time.Now()
for k, v := range raw {
if now.Sub(v.Created) < cacheTTL*2 {
cache[k] = cacheEntry{Body: v.Body, Created: v.Created}
}
}
log.Printf("cache cargada: %d entradas", len(cache))
}

func saveCache() {
for {
time.Sleep(60 * time.Second)
mu.Lock()
if !dirty {
mu.Unlock()
continue
}
snap := make(map[string]struct {
Body    []byte    `json:"b"`
Created time.Time `json:"t"`
}, len(cache))
cut := time.Now().Add(-cacheTTL * 2)
for k, v := range cache {
if v.Created.After(cut) {
snap[k] = struct {
Body    []byte    `json:"b"`
Created time.Time `json:"t"`
}{v.Body, v.Created}
}
}
dirty = false
mu.Unlock()

data, err := json.Marshal(snap)
if err != nil {
continue
}
if err := os.WriteFile(cachePath+".tmp", data, 0644); err == nil {
os.Rename(cachePath+".tmp", cachePath)
}
}
}

func cors(w http.ResponseWriter) {
w.Header().Set("Access-Control-Allow-Origin", "*")
w.Header().Set("Access-Control-Allow-Headers", "X-API-Key, Content-Type")
w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
cors(w)
w.Header().Set("Content-Type", "application/json; charset=utf-8")
w.WriteHeader(code)
json.NewEncoder(w).Encode(v)
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
if apiKey != "" {
if r.URL.Query().Get("key") != apiKey && r.Header.Get("X-API-Key") != apiKey {
writeJSON(w, 401, map[string]string{"error": "api key invalida o ausente"})
return
}
}

q := strings.TrimSpace(r.URL.Query().Get("q"))
if q == "" {
writeJSON(w, 400, map[string]string{"error": "falta el parametro q"})
return
}

limit := defLimit
if v := r.URL.Query().Get("limit"); v != "" {
n, err := strconv.Atoi(v)
if err != nil {
writeJSON(w, 400, map[string]string{"error": "limit debe ser un numero"})
return
}
limit = n
}
if limit < 1 {
limit = 1
}
if limit > maxLimit {
limit = maxLimit
}

params := url.Values{}
params.Set("q", q)
params.Set("format", "json")
lang := r.URL.Query().Get("lang")
if lang == "" {
lang = "es"
}
params.Set("language", lang)
for _, k := range []string{"category", "engines", "time_range", "pageno", "safesearch"} {
if v := r.URL.Query().Get(k); v != "" {
params.Set(k, v)
}
}

key := params.Encode() + "|" + strconv.Itoa(limit)
start := time.Now()

mu.Lock()
stats.Requests++
mu.Unlock()

mu.RLock()
e, hit := cache[key]
mu.RUnlock()
if hit && time.Since(e.Created) < cacheTTL {
mu.Lock()
stats.CacheHits++
mu.Unlock()
// cached/took_ms se recalculan: el body guardado es el original
var out map[string]any
if json.Unmarshal(e.Body, &out) == nil {
out["cached"] = true
out["took_ms"] = int(time.Since(start).Milliseconds())
if fixed, err := json.Marshal(out); err == nil {
cors(w)
w.Header().Set("Content-Type", "application/json; charset=utf-8")
w.Header().Set("X-Cache", "HIT")
w.WriteHeader(200)
w.Write(fixed)
return
}
}
}

client := &http.Client{Timeout: upTimeout}
resp, err := client.Get(searxURL + "/search?" + params.Encode())
if err != nil {
mu.Lock()
stats.Errors++
mu.Unlock()
writeJSON(w, 502, map[string]string{"error": "fallo la consulta: " + err.Error()})
return
}
defer resp.Body.Close()

var upstream struct {
Query               string           `json:"query"`
Results             []map[string]any `json:"results"`
Suggestions         []string         `json:"suggestions"`
UnresponsiveEngines [][]string       `json:"unresponsive_engines"`
}
if err := json.NewDecoder(resp.Body).Decode(&upstream); err != nil {
mu.Lock()
stats.Errors++
mu.Unlock()
writeJSON(w, 502, map[string]string{"error": "respuesta invalida de searxng"})
return
}

if len(upstream.Results) > limit {
upstream.Results = upstream.Results[:limit]
}
if upstream.Suggestions == nil {
upstream.Suggestions = []string{}
}
if upstream.UnresponsiveEngines == nil {
upstream.UnresponsiveEngines = [][]string{}
}

payload := map[string]any{
"query":                upstream.Query,
"count":                len(upstream.Results),
"limit":                limit,
"results":              upstream.Results,
"suggestions":          upstream.Suggestions,
"unresponsive_engines": upstream.UnresponsiveEngines,
"cached":               false,
"took_ms":              int(time.Since(start).Milliseconds()),
}
body, _ := json.Marshal(payload)

if len(upstream.Results) > 0 {
mu.Lock()
cache[key] = cacheEntry{Body: body, Created: time.Now()}
dirty = true
mu.Unlock()
}

cors(w)
w.Header().Set("Content-Type", "application/json; charset=utf-8")
w.Header().Set("X-Cache", "MISS")
w.WriteHeader(200)
w.Write(body)
}

func main() {
os.MkdirAll("/data", 0755)
loadCache()
go saveCache()

http.HandleFunc("/search", handleSearch)
http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
writeJSON(w, 200, map[string]any{"status": "ok", "cache_ttl": int(cacheTTL.Seconds())})
})
http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
mu.RLock()
s := stats
mu.RUnlock()
writeJSON(w, 200, s)
})
http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
if r.URL.Path != "/" {
writeJSON(w, 404, map[string]any{"error": "ruta no encontrada", "paths": []string{"/search", "/health", "/stats"}})
return
}
http.ServeFile(w, r, "/app/public/index.html")
})

addr := ":" + port
fmt.Printf("api-go escuchando en %s -> %s\n", addr, searxURL)
log.Fatal(http.ListenAndServe(addr, nil))
}
