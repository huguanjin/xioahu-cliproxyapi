package management

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// proxyProbeURL is the unauthenticated upstream endpoint used to check that a proxy can reach
// Google. Any HTTP response counts as reachable; only a transport failure marks the proxy bad.
var proxyProbeURL = antigravity.APIEndpoint + "/"

const (
	// proxyProbeTimeout bounds one connectivity probe so a dead proxy cannot hang a request.
	proxyProbeTimeout = 10 * time.Second
	// proxyProbeConcurrency caps how many proxies are probed at the same time.
	proxyProbeConcurrency = 8
)

// proxyPoolEntryView is a proxy pool entry plus how many credentials are currently bound to it.
type proxyPoolEntryView struct {
	config.ProxyPoolEntry
	BoundCount int `json:"bound-count"`
}

// proxyProbeResult is the outcome of one connectivity probe through a proxy.
type proxyProbeResult struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	OK         bool   `json:"ok"`
	LatencyMs  int64  `json:"latency-ms"`
	StatusCode int    `json:"status-code,omitempty"`
	Error      string `json:"error,omitempty"`
}

// proxyBoundCounts counts registered credentials per proxy URL, whatever their provider or state.
func (h *Handler) proxyBoundCounts() map[string]int {
	counts := make(map[string]int)
	if h == nil || h.authManager == nil {
		return counts
	}
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			counts[proxyURL]++
		}
	}
	return counts
}

// probeProxy sends one request to proxyProbeURL through proxyURL and reports reachability.
func probeProxy(ctx context.Context, proxyURL string) proxyProbeResult {
	var res proxyProbeResult
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		res.Error = errBuild.Error()
		return res
	}
	if transport == nil {
		res.Error = "proxy url is empty"
		return res
	}
	defer transport.CloseIdleConnections()

	probeCtx, cancel := context.WithTimeout(ctx, proxyProbeTimeout)
	defer cancel()
	req, errReq := http.NewRequestWithContext(probeCtx, http.MethodGet, proxyProbeURL, nil)
	if errReq != nil {
		res.Error = errReq.Error()
		return res
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	start := time.Now()
	resp, errDo := client.Do(req)
	res.LatencyMs = time.Since(start).Milliseconds()
	if errDo != nil {
		res.Error = errDo.Error()
		return res
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if errClose := resp.Body.Close(); errClose != nil {
		log.Debugf("proxy probe: close response body: %v", errClose)
	}
	res.StatusCode = resp.StatusCode
	res.OK = true
	return res
}

// probeProxies probes every entry with bounded concurrency; results keep the input order.
func probeProxies(ctx context.Context, entries []config.ProxyPoolEntry) []proxyProbeResult {
	results := make([]proxyProbeResult, len(entries))
	sem := make(chan struct{}, proxyProbeConcurrency)
	var wg sync.WaitGroup
	for i := range entries {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			res := probeProxy(ctx, entries[i].ProxyURL)
			res.ID = entries[i].ID
			res.Name = entries[i].Name
			results[i] = res
		}(i)
	}
	wg.Wait()
	return results
}

// TestProxyPool probes proxy connectivity. The body selects the target: "id" tests one pool entry,
// "proxy-url" tests an arbitrary address (for an entry not saved yet), "all" tests the whole pool.
func (h *Handler) TestProxyPool(c *gin.Context) {
	var body struct {
		ID       string `json:"id"`
		ProxyURL string `json:"proxy-url"`
		All      bool   `json:"all"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	var targets []config.ProxyPoolEntry
	switch {
	case strings.TrimSpace(body.ProxyURL) != "":
		targets = []config.ProxyPoolEntry{{ID: strings.TrimSpace(body.ID), ProxyURL: strings.TrimSpace(body.ProxyURL)}}
	case strings.TrimSpace(body.ID) != "":
		id := strings.TrimSpace(body.ID)
		h.mu.Lock()
		for _, entry := range h.cfg.ProxyPool {
			if entry.ID == id {
				targets = []config.ProxyPoolEntry{entry}
				break
			}
		}
		h.mu.Unlock()
		if len(targets) == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
			return
		}
	case body.All:
		h.mu.Lock()
		targets = append(targets, h.cfg.ProxyPool...)
		h.mu.Unlock()
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "id, proxy-url or all is required"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"results": probeProxies(c.Request.Context(), targets)})
}

// pickLeastLoaded returns the index of the proxy with the fewest bound credentials. Ties go to the
// earlier entry so repeated runs over the same input are deterministic. It returns -1 for no proxies.
func pickLeastLoaded(proxyURLs []string, loads map[string]int) int {
	best := -1
	for i, proxyURL := range proxyURLs {
		if best == -1 || loads[proxyURL] < loads[proxyURLs[best]] {
			best = i
		}
	}
	return best
}

var errNoProxyPool = errors.New("proxy pool is empty")

// proxyAssigner spreads uploaded credentials evenly over the reachable proxies of the pool.
// Its running load starts from what is already bound, so a second import tops up the least
// loaded proxies instead of piling onto the first ones.
type proxyAssigner struct {
	h         *Handler
	proxies   []config.ProxyPoolEntry
	loads     map[string]int
	assigned  map[string]int
	unhealthy []proxyProbeResult
}

type proxyAssignCount struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Assigned int    `json:"assigned"`
	Bound    int    `json:"bound"`
}

type proxyAssignReport struct {
	Distribution []proxyAssignCount `json:"distribution"`
	Unhealthy    []proxyProbeResult `json:"unhealthy,omitempty"`
}

// newProxyAssigner probes the pool and keeps only reachable proxies. It fails when none is usable
// so that nothing gets uploaded without a proxy the operator asked for.
func (h *Handler) newProxyAssigner(ctx context.Context) (*proxyAssigner, error) {
	h.mu.Lock()
	pool := append([]config.ProxyPoolEntry(nil), h.cfg.ProxyPool...)
	h.mu.Unlock()
	if len(pool) == 0 {
		return nil, errNoProxyPool
	}

	a := &proxyAssigner{
		h:        h,
		loads:    h.proxyBoundCounts(),
		assigned: make(map[string]int),
	}
	for i, res := range probeProxies(ctx, pool) {
		if res.OK {
			a.proxies = append(a.proxies, pool[i])
		} else {
			a.unhealthy = append(a.unhealthy, res)
		}
	}
	if len(a.proxies) == 0 {
		return nil, fmt.Errorf("none of the %d proxies in the pool is reachable", len(pool))
	}
	return a, nil
}

func (a *proxyAssigner) bump(proxyURL string, delta int) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return
	}
	if a.loads[proxyURL]+delta < 0 {
		a.loads[proxyURL] = 0
		return
	}
	a.loads[proxyURL] += delta
}

// apply returns the credential JSON with a proxy_url filled in. An explicit proxy_url in the file wins,
// then the proxy an existing credential of the same name already uses (re-uploading must not move an
// account to another IP); only otherwise is the least loaded reachable proxy picked.
func (a *proxyAssigner) apply(name string, data []byte) []byte {
	if !gjson.ValidBytes(data) || !gjson.ParseBytes(data).IsObject() {
		return data
	}
	existingURL := ""
	if existing := a.h.findAuthForDelete(name); existing != nil {
		existingURL = strings.TrimSpace(existing.ProxyURL)
	}

	if explicit := strings.TrimSpace(gjson.GetBytes(data, "proxy_url").String()); explicit != "" {
		a.bump(existingURL, -1)
		a.bump(explicit, 1)
		return data
	}
	proxyURL := existingURL
	if proxyURL == "" {
		urls := make([]string, len(a.proxies))
		for i, entry := range a.proxies {
			urls[i] = strings.TrimSpace(entry.ProxyURL)
		}
		chosen := a.proxies[pickLeastLoaded(urls, a.loads)]
		proxyURL = strings.TrimSpace(chosen.ProxyURL)
		a.bump(proxyURL, 1)
		a.assigned[chosen.ID]++
	}
	out, errSet := sjson.SetBytes(data, "proxy_url", proxyURL)
	if errSet != nil {
		log.Warnf("auto proxy: set proxy_url for %s failed: %v", name, errSet)
		return data
	}
	return out
}

// report summarizes the distribution over every reachable proxy plus the ones that were skipped.
func (a *proxyAssigner) report() proxyAssignReport {
	rep := proxyAssignReport{
		Distribution: make([]proxyAssignCount, 0, len(a.proxies)),
		Unhealthy:    a.unhealthy,
	}
	for _, entry := range a.proxies {
		rep.Distribution = append(rep.Distribution, proxyAssignCount{
			ID:       entry.ID,
			Name:     entry.Name,
			Assigned: a.assigned[entry.ID],
			Bound:    a.loads[strings.TrimSpace(entry.ProxyURL)],
		})
	}
	return rep
}
