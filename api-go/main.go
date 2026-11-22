package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type cacheEntry struct {
	Body    []byte
	Created time.Time
}

type Stats struct {
	Requests  int64 `json:"requests"`
	CacheHits int64 `json:"cache_hits"`
	Errors    int64 `json:"errors"`
}

var (
	searxURL  = env("SEARX_URL", "http://searxng:8080")
	port      = env("PORT", "8080")
	defLimit  = envInt("DEFAULT_LIMIT", 20)
	maxLimit  = envInt("MAX_LIMIT", 50)
	cacheTTL  = time.Duration(envInt("CACHE_TTL", 3600)) * time.Second
	apiKey    = os.Getenv("API_KEY")
	upTimeout = time.Duration(envInt("UPSTREAM_TIMEOUT", 25)) * time.Second
	cachePath = env("CACHE_FILE", "/data/cache.json")
	minRel    = envFloat("MIN_RELEVANCE", 0.34)

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

func envFloat(k string, d float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
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
		cut := time.Now().Add(-cacheTTL * 2)
		snap := make(map[string]cacheEntry, len(cache))
		for k, v := range cache {
			if v.Created.After(cut) {
				snap[k] = v
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

var stopwords = map[string]bool{
	"de": true, "la": true, "el": true, "los": true, "las": true, "un": true,
	"una": true, "y": true, "o": true, "en": true, "del": true, "al": true,
	"que": true, "por": true, "para": true, "con": true, "the": true, "a": true,
	"of": true, "to": true, "in": true, "and": true, "is": true, "mi": true,
	"tu": true, "su": true, "es": true, "son": true, "como": true,
}

func tokenize(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			w := b.String()
			if len(w) >= 2 && !stopwords[w] {
				out = append(out, w)
			}
			b.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// variants agrega una raiz aproximada para que "clubes" matchee "club".
func variants(term string) []string {
	v := []string{term}
	if len(term) > 5 && strings.HasSuffix(term, "es") {
		v = append(v, term[:len(term)-2])
	} else if len(term) > 4 && strings.HasSuffix(term, "s") {
		v = append(v, term[:len(term)-1])
	}
	return v
}

// relevance devuelve la fraccion de terminos de la consulta presentes
// en el resultado. 0 = nada que ver, 1 = todos los terminos aparecen.
func relevance(title, content, link, rawQuery string) float64 {
	terms := tokenize(rawQuery)
	if len(terms) == 0 {
		return 1
	}
	hay := strings.ToLower(title + " " + content + " " + link)
	matched := 0
	for _, t := range terms {
		for _, v := range variants(t) {
			if strings.Contains(hay, v) {
				matched++
				break
			}
		}
	}
	return float64(matched) / float64(len(terms))
}

type scored struct {
	raw map[string]any
	rel float64
	eng string
	sc  float64
}

// filterResults descarta resultados irrelevantes y detecta motores
// intoxicados. Un motor bloqueado responde con HTML generico o cache
// de otras consultas: ahi casi todo el lote es irrelevante.
func filterResults(results []map[string]any, rawQuery string, strict bool) ([]map[string]any, map[string]int, int) {
	if len(results) == 0 {
		return results, map[string]int{}, 0
	}

	rs := make([]scored, 0, len(results))
	for _, r := range results {
		title, _ := r["title"].(string)
		content, _ := r["content"].(string)
		link, _ := r["url"].(string)
		eng, _ := r["engine"].(string)
		sc, _ := r["score"].(float64)
		rs = append(rs, scored{
			raw: r,
			rel: relevance(title, content, link, rawQuery),
			eng: eng,
			sc:  sc,
		})
	}

	if !strict {
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].sc > rs[j].sc })
		out := make([]map[string]any, len(rs))
		for i := range rs {
			out[i] = rs[i].raw
		}
		return out, map[string]int{}, 0
	}

	type agg struct{ total, good int }
	byEng := map[string]*agg{}
	for _, r := range rs {
		a := byEng[r.eng]
		if a == nil {
			a = &agg{}
			byEng[r.eng] = a
		}
		a.total++
		if r.rel >= minRel {
			a.good++
		}
	}

	droppedEngines := map[string]int{}
	poisoned := map[string]bool{}
	for name, a := range byEng {
		// 3+ resultados con menos de 30% relevantes: motor roto
		if a.total >= 3 && float64(a.good)/float64(a.total) < 0.3 {
			poisoned[name] = true
			droppedEngines[name] = a.total
		}
	}

	droppedResults := 0
	kept := make([]scored, 0, len(rs))
	for _, r := range rs {
		if poisoned[r.eng] {
			continue
		}
		if r.rel < minRel {
			droppedResults++
			continue
		}
		kept = append(kept, r)
	}

	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].rel != kept[j].rel {
			return kept[i].rel > kept[j].rel
		}
		return kept[i].sc > kept[j].sc
	})

	out := make([]map[string]any, len(kept))
	for i := range kept {
		out[i] = kept[i].raw
	}
	return out, droppedEngines, droppedResults
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

	strict := r.URL.Query().Get("strict") != "0"

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

	key := params.Encode() + "|" + strconv.Itoa(limit) + "|" + strconv.FormatBool(strict)
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

	rawCount := len(upstream.Results)
	kept, droppedEngines, droppedResults := filterResults(upstream.Results, q, strict)
	if len(kept) > limit {
		kept = kept[:limit]
	}
	if upstream.Suggestions == nil {
		upstream.Suggestions = []string{}
	}
	if upstream.UnresponsiveEngines == nil {
		upstream.UnresponsiveEngines = [][]string{}
	}

	payload := map[string]any{
		"query":                upstream.Query,
		"count":                len(kept),
		"limit":                limit,
		"results":              kept,
		"suggestions":          upstream.Suggestions,
		"unresponsive_engines": upstream.UnresponsiveEngines,
		"filtered": map[string]any{
			"raw_results":     rawCount,
			"dropped_results": droppedResults,
			"dropped_engines": droppedEngines,
		},
		"cached":  false,
		"took_ms": int(time.Since(start).Milliseconds()),
	}
	body, _ := json.Marshal(payload)

	if len(kept) > 0 {
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
		writeJSON(w, 200, map[string]any{
			"status":        "ok",
			"cache_ttl":     int(cacheTTL.Seconds()),
			"min_relevance": minRel,
		})
	})
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		s := stats
		mu.RUnlock()
		writeJSON(w, 200, s)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeJSON(w, 404, map[string]any{
				"error": "ruta no encontrada",
				"paths": []string{"/search", "/health", "/stats"},
			})
			return
		}
		http.ServeFile(w, r, "/app/public/index.html")
	})

	addr := ":" + port
	fmt.Printf("api-go escuchando en %s -> %s (relevancia min %.2f)\n", addr, searxURL, minRel)
	log.Fatal(http.ListenAndServe(addr, nil))
}
