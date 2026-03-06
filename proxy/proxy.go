package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Upstream Uniswap API targets
var upstreams = map[string]upstream{
	"/uniswap/":    {base: "https://api.uniswap.org", stripPrefix: "/uniswap"},
	"/liquidity/":  {base: "https://liquidity.backend-prod.api.uniswap.org", stripPrefix: "/liquidity"},
	"/gateway/":    {base: "https://interface.gateway.uniswap.org", stripPrefix: "/gateway"},
	"/conversion/": {base: "https://entry-gateway.backend-prod.api.uniswap.org", stripPrefix: "/conversion"},
}

type upstream struct {
	base        string
	stripPrefix string
}

type Config struct {
	RedisURL string
}

type Proxy struct {
	client *http.Client
	cache  *memCache
}

func New(cfg Config) *Proxy {
	return &Proxy{
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		cache: newMemCache(),
	}
}

func (p *Proxy) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", p.handleHealth)

	// Sort prefixes by length descending so longer prefixes match first
	prefixes := make([]string, 0, len(upstreams))
	for prefix := range upstreams {
		prefixes = append(prefixes, prefix)
	}
	sort.Slice(prefixes, func(i, j int) bool {
		return len(prefixes[i]) > len(prefixes[j])
	})

	for _, prefix := range prefixes {
		up := upstreams[prefix]
		mux.HandleFunc(prefix, p.makeHandler(up))
	}
}

func (p *Proxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func (p *Proxy) makeHandler(up upstream) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// CORS headers — this is the whole point
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, Origin, Accept")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Build upstream URL
		path := strings.TrimPrefix(r.URL.Path, up.stripPrefix)
		targetURL := up.base + path
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		// Check cache for GET requests
		if r.Method == http.MethodGet {
			cacheKey := cacheKeyFor(targetURL, "")
			if entry, ok := p.cache.get(cacheKey); ok {
				w.Header().Set("Content-Type", entry.contentType)
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(entry.status)
				w.Write(entry.body)
				return
			}
		}

		// Read request body for POST (needed for cache key + forwarding)
		var bodyBytes []byte
		if r.Body != nil {
			bodyBytes, _ = io.ReadAll(r.Body)
			r.Body.Close()
		}

		// Check cache for POST (GraphQL etc)
		var cacheKey string
		if r.Method == http.MethodPost {
			cacheKey = cacheKeyFor(targetURL, string(bodyBytes))
			if entry, ok := p.cache.get(cacheKey); ok {
				w.Header().Set("Content-Type", entry.contentType)
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(entry.status)
				w.Write(entry.body)
				return
			}
		}

		// Forward request upstream
		var bodyReader io.Reader
		if len(bodyBytes) > 0 {
			bodyReader = strings.NewReader(string(bodyBytes))
		}

		req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bodyReader)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Forward relevant headers
		for _, h := range []string{"Content-Type", "Accept", "Authorization", "X-Api-Key", "X-Request-Id"} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		// Set origin to look like a browser request from our domain
		req.Header.Set("Origin", "https://lux.exchange")

		resp, err := p.client.Do(req)
		if err != nil {
			log.Printf("upstream error %s: %v", targetURL, err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusBadGateway)
			return
		}

		// Cache successful responses
		ct := resp.Header.Get("Content-Type")
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			ttl := cacheTTL(up.stripPrefix, r.URL.Path)
			if ttl > 0 {
				key := cacheKey
				if key == "" {
					key = cacheKeyFor(targetURL, "")
				}
				p.cache.set(key, cacheEntry{
					body:        respBody,
					contentType: ct,
					status:      resp.StatusCode,
				}, ttl)
			}
		}

		// Write response
		w.Header().Set("Content-Type", ct)
		w.Header().Set("X-Cache", "MISS")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
	}
}

// cacheTTL returns cache duration based on the endpoint pattern.
func cacheTTL(prefix, path string) time.Duration {
	p := prefix + path

	// Token lists, supported chains — cache 5 min
	if strings.Contains(p, "/tokens") || strings.Contains(p, "/chains") || strings.Contains(p, "/supportedChains") {
		return 5 * time.Minute
	}

	// Pool data — cache 30s
	if strings.Contains(p, "/pools") || strings.Contains(p, "/positions") {
		return 30 * time.Second
	}

	// Quotes — cache 5s (prices change fast)
	if strings.Contains(p, "/quote") || strings.Contains(p, "/price") {
		return 5 * time.Second
	}

	// GraphQL queries — cache 10s
	if strings.Contains(p, "/graphql") {
		return 10 * time.Second
	}

	// Default: cache 15s
	return 15 * time.Second
}

func cacheKeyFor(url, body string) string {
	h := sha256.New()
	h.Write([]byte(url))
	if body != "" {
		h.Write([]byte(body))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// In-memory TTL cache
type cacheEntry struct {
	body        []byte
	contentType string
	status      int
	expiresAt   time.Time
}

type memCache struct {
	mu    sync.RWMutex
	items map[string]cacheEntry
}

func newMemCache() *memCache {
	c := &memCache{items: make(map[string]cacheEntry)}
	go c.janitor()
	return c
}

func (c *memCache) get(key string) (cacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.items[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return cacheEntry{}, false
	}
	return entry, true
}

func (c *memCache) set(key string, entry cacheEntry, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.expiresAt = time.Now().Add(ttl)
	c.items[key] = entry
}

func (c *memCache) janitor() {
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		c.mu.Lock()
		now := time.Now()
		for k, v := range c.items {
			if now.After(v.expiresAt) {
				delete(c.items, k)
			}
		}
		c.mu.Unlock()
	}
}
