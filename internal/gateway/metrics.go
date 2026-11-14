package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/gpu"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/runtime"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
)

// metrics keeps Prometheus counters and histograms in memory (reset when the gateway restarts,
// which Prometheus rate() handles); the usage database is the durable record.
type metrics struct {
	mu        sync.Mutex
	requests  map[[4]string]int64 // model, engine, api, status
	tokens    map[[3]string]int64 // model, engine, kind
	ttft      map[string]*histogram
	duration  map[string]*histogram
	failovers map[string]int64      // instance
	restarts  map[string]int64      // model
	evictions map[[2]string]int64   // evicted model, for model
	flags     map[[3]string]int64   // rule, severity, action
	category  map[[2]string]int64   // model, category
	errorKind map[[2]string]int64   // model, error kind
	cost      map[[2]string]float64 // model, team
	gpus      []gpu.GPU
	instances []store.InstanceSample
	// unrouted holds the model names of requests that never reached an engine. Clients choose those names,
	// so only the first maxUnroutedModels get their own series; later ones are counted as "other".
	unrouted map[string]struct{}
	// routed holds the names the gateway found a model for: a request that fails before reaching the
	// engine (queue full, start failed) still counts under its model.
	routed map[string]struct{}

	// unstoredDenials counts refused-key (401) requests over the stored-per-minute limit.
	unstoredDenials int64
}

var latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

type histogram struct {
	counts []int64 // per bucket, cumulative at render
	sum    float64
	n      int64
}

func (h *histogram) observe(v float64) {
	if h.counts == nil {
		h.counts = make([]int64, len(latencyBuckets))
	}
	for i, b := range latencyBuckets {
		if v <= b {
			h.counts[i]++
			break
		}
	}
	h.sum += v
	h.n++
}

func newMetrics() *metrics {
	return &metrics{requests: map[[4]string]int64{}, tokens: map[[3]string]int64{}, ttft: map[string]*histogram{},
		duration: map[string]*histogram{}, failovers: map[string]int64{}, restarts: map[string]int64{},
		evictions: map[[2]string]int64{}, flags: map[[3]string]int64{}, category: map[[2]string]int64{},
		errorKind: map[[2]string]int64{}, cost: map[[2]string]float64{}}
}

// markRouted records that name is a model the gateway serves.
func (m *metrics) markRouted(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.routed == nil {
		m.routed = map[string]struct{}{}
	}
	m.routed[name] = struct{}{}
}

// maxUnroutedModels bounds the metric series a client can create by asking for models that do not exist.
const maxUnroutedModels = 200

func (m *metrics) observe(r *store.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, real := m.routed[r.Model]; !real && r.Engine == "" && r.Model != "" {
		if _, seen := m.unrouted[r.Model]; !seen {
			if len(m.unrouted) >= maxUnroutedModels {
				folded := *r
				folded.Model = "other"
				r = &folded
			} else {
				if m.unrouted == nil {
					m.unrouted = map[string]struct{}{}
				}
				m.unrouted[r.Model] = struct{}{}
			}
		}
	}
	m.requests[[4]string{r.Model, r.Engine, r.API, fmt.Sprint(r.Status)}]++
	for _, f := range r.Flags {
		m.flags[[3]string{f.Rule, f.Severity, f.Action}]++
	}
	if r.Category != "" {
		m.category[[2]string{r.Model, r.Category}]++
	}
	if r.ErrorKind != "" {
		m.errorKind[[2]string{r.Model, r.ErrorKind}]++
	}
	if r.Cost != nil {
		m.cost[[2]string{r.Model, r.Team}] += *r.Cost
	}
	add := func(kind string, v *int64) {
		if v != nil {
			m.tokens[[3]string{r.Model, r.Engine, kind}] += *v
		}
	}
	add("prompt", r.PromptTokens)
	add("completion", r.CompletionTokens)
	add("cached", r.CachedTokens)
	add("reasoning", r.ReasoningTokens)
	hist := func(hs map[string]*histogram, ms *int64) {
		if ms == nil || r.Status >= 400 {
			return
		}
		h := hs[r.Model]
		if h == nil {
			h = &histogram{}
			hs[r.Model] = h
		}
		h.observe(float64(*ms) / 1000)
	}
	hist(m.ttft, r.TTFTMS)
	hist(m.duration, r.DurationMS)
}

func (m *metrics) denialUnstored() {
	m.mu.Lock()
	m.unstoredDenials++
	m.mu.Unlock()
}

func (m *metrics) failover(in *runtime.Instance) {
	m.mu.Lock()
	m.failovers[in.ID]++
	m.mu.Unlock()
}

func (m *metrics) evicted(model, forModel string) {
	m.mu.Lock()
	m.evictions[[2]string{model, forModel}]++
	m.mu.Unlock()
}

func (m *metrics) restarted(in *runtime.Instance) {
	m.mu.Lock()
	m.restarts[in.Name]++
	m.mu.Unlock()
}

func (m *metrics) setSamples(gs []gpu.GPU, is []store.InstanceSample) {
	m.mu.Lock()
	m.gpus, m.instances = gs, is
	m.mu.Unlock()
}

func esc(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

type promWriter struct {
	b    strings.Builder
	seen map[string]bool
}

func (p *promWriter) head(name, typ, help string) {
	if p.seen[name] {
		return
	}
	p.seen[name] = true
	fmt.Fprintf(&p.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func (p *promWriter) sample(name string, labels []string, v float64) {
	p.b.WriteString(name)
	if len(labels) > 0 {
		p.b.WriteByte('{')
		for i := 0; i < len(labels); i += 2 {
			if i > 0 {
				p.b.WriteByte(',')
			}
			fmt.Fprintf(&p.b, `%s="%s"`, labels[i], esc(labels[i+1]))
		}
		p.b.WriteByte('}')
	}
	fmt.Fprintf(&p.b, " %g\n", v)
}

func sortedKeys2(m map[[2]string]int64) [][2]string {
	out := make([][2]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] || out[i][0] == out[j][0] && out[i][1] < out[j][1] })
	return out
}

func sortedKeys3(m map[[3]string]int64) [][3]string {
	out := make([][3]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i][:], "\x00") < strings.Join(out[j][:], "\x00") })
	return out
}

func sortedHist(hs map[string]*histogram) []string {
	ks := make([]string, 0, len(hs))
	for k := range hs {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// serveMetrics renders the Prometheus text exposition format.
func (g *Gateway) serveMetrics(w http.ResponseWriter, r *http.Request) {
	p := &promWriter{seen: map[string]bool{}}
	m := g.metrics
	m.mu.Lock()
	reqKeys := make([][4]string, 0, len(m.requests))
	for k := range m.requests {
		reqKeys = append(reqKeys, k)
	}
	sort.Slice(reqKeys, func(i, j int) bool { return fmt.Sprint(reqKeys[i]) < fmt.Sprint(reqKeys[j]) })
	p.head("emx_requests_total", "counter", "Requests through the gateway.")
	for _, k := range reqKeys {
		p.sample("emx_requests_total", []string{"model", k[0], "engine", k[1], "api", k[2], "status", k[3]}, float64(m.requests[k]))
	}
	tokKeys := make([][3]string, 0, len(m.tokens))
	for k := range m.tokens {
		tokKeys = append(tokKeys, k)
	}
	sort.Slice(tokKeys, func(i, j int) bool { return fmt.Sprint(tokKeys[i]) < fmt.Sprint(tokKeys[j]) })
	p.head("emx_tokens_total", "counter", "Tokens reported by engines (kind: prompt, completion, cached, reasoning).")
	for _, k := range tokKeys {
		p.sample("emx_tokens_total", []string{"model", k[0], "engine", k[1], "kind", k[2]}, float64(m.tokens[k]))
	}
	p.head("emx_policy_flags_total", "counter", "Policy rule matches (one per rule and detector per request).")
	for _, k := range sortedKeys3(m.flags) {
		p.sample("emx_policy_flags_total", []string{"rule", k[0], "severity", k[1], "action", k[2]}, float64(m.flags[k]))
	}
	p.head("emx_requests_by_category_total", "counter", "Requests by prompt category.")
	for _, k := range sortedKeys2(m.category) {
		p.sample("emx_requests_by_category_total", []string{"model", k[0], "category", k[1]}, float64(m.category[k]))
	}
	p.head("emx_errors_total", "counter", "Failed requests by error kind.")
	for _, k := range sortedKeys2(m.errorKind) {
		p.sample("emx_errors_total", []string{"model", k[0], "kind", k[1]}, float64(m.errorKind[k]))
	}
	p.head("emx_cost_total", "counter", "Cost of metered requests in pricing.currency (team empty for keys without one).")
	costKeys := make([][2]string, 0, len(m.cost))
	for k := range m.cost {
		costKeys = append(costKeys, k)
	}
	sort.Slice(costKeys, func(i, j int) bool {
		return costKeys[i][0]+"\x00"+costKeys[i][1] < costKeys[j][0]+"\x00"+costKeys[j][1]
	})
	for _, k := range costKeys {
		p.sample("emx_cost_total", []string{"model", k[0], "team", k[1]}, m.cost[k])
	}
	for _, h := range []struct {
		name, help string
		hs         map[string]*histogram
	}{
		{"emx_ttft_seconds", "Time to first streamed token.", m.ttft},
		{"emx_request_duration_seconds", "Request duration, including queueing and autostart.", m.duration},
	} {
		p.head(h.name, "histogram", h.help)
		for _, model := range sortedHist(h.hs) {
			hist := h.hs[model]
			var cum int64
			for i, b := range latencyBuckets {
				cum += hist.counts[i]
				p.sample(h.name+"_bucket", []string{"model", model, "le", fmt.Sprint(b)}, float64(cum))
			}
			p.sample(h.name+"_bucket", []string{"model", model, "le", "+Inf"}, float64(hist.n))
			p.sample(h.name+"_sum", []string{"model", model}, hist.sum)
			p.sample(h.name+"_count", []string{"model", model}, float64(hist.n))
		}
	}
	p.head("emx_failovers_total", "counter", "Requests moved to another replica after an instance failed.")
	for id, n := range m.failovers {
		p.sample("emx_failovers_total", []string{"instance", id}, float64(n))
	}
	p.head("emx_restarts_total", "counter", "Automatic restarts after crashes or failed health checks.")
	for model, n := range m.restarts {
		p.sample("emx_restarts_total", []string{"model", model}, float64(n))
	}
	gs, samples := m.gpus, m.instances
	m.mu.Unlock()

	p.head("emx_gpu_memory_used_mib", "gauge", "GPU memory in use (last sample).")
	p.head("emx_gpu_memory_total_mib", "gauge", "GPU memory size.")
	p.head("emx_gpu_utilization_ratio", "gauge", "GPU utilization 0-1 (last sample).")
	for _, x := range gs {
		l := []string{"gpu", fmt.Sprint(x.Index), "name", x.Name}
		p.sample("emx_gpu_memory_used_mib", l, float64(x.MemUsedMiB))
		p.sample("emx_gpu_memory_total_mib", l, float64(x.MemTotalMiB))
		p.sample("emx_gpu_utilization_ratio", l, float64(x.UtilPct)/100)
	}
	p.head("emx_instance_gpu_memory_mib", "gauge", "GPU memory held by an instance's processes (last sample).")
	p.head("emx_instance_rss_mib", "gauge", "Resident memory of an instance's processes (last sample).")
	for _, s := range samples {
		l := []string{"instance", s.Instance, "engine", s.Engine, "model", s.Model}
		p.sample("emx_instance_gpu_memory_mib", l, float64(s.GPUMemMiB))
		p.sample("emx_instance_rss_mib", l, float64(s.RSSMiB))
	}

	ins, _ := runtime.List()
	p.head("emx_instance_up", "gauge", "1 when the instance is ready to serve.")
	p.head("emx_instance_inflight_requests", "gauge", "Requests in flight per instance.")
	g.activeMu.Lock()
	active := map[string]int{}
	for k, v := range g.active {
		active[k] = v
	}
	dropped := g.dropped
	g.activeMu.Unlock()
	for _, in := range ins {
		st := g.instanceStatus(r.Context(), in)
		up := 0.0
		if st == runtime.StatusReady {
			up = 1
		}
		l := []string{"instance", in.ID, "engine", in.Engine, "model", in.Name, "owner", in.Owner, "status", st}
		p.sample("emx_instance_up", l, up)
		p.sample("emx_instance_inflight_requests", []string{"instance", in.ID, "model", in.Name}, float64(active[in.ID]))
	}
	g.activeMu.Lock()
	waitingFor := map[string]int{}
	for k, v := range g.waiting {
		waitingFor[k] = v
	}
	queueLen := len(g.swapQueue)
	g.activeMu.Unlock()
	p.head("emx_model_waiting_requests", "gauge", "Requests waiting for their model to be started or swapped in.")
	for model, n := range waitingFor {
		p.sample("emx_model_waiting_requests", []string{"model", model}, float64(n))
	}
	p.head("emx_swap_queue_length", "gauge", "Starts waiting for devices other models hold.")
	p.sample("emx_swap_queue_length", nil, float64(queueLen))
	m.mu.Lock()
	p.head("emx_evictions_total", "counter", "Instances stopped to make room for another model.")
	for k, n := range m.evictions {
		p.sample("emx_evictions_total", []string{"model", k[0], "for", k[1]}, float64(n))
	}
	m.mu.Unlock()
	p.head("emx_queue_waiting", "gauge", "Requests waiting for a concurrency slot per model.")
	for model, n := range g.lim.queued() {
		p.sample("emx_queue_waiting", []string{"model", model}, float64(n))
	}
	p.head("emx_usage_log_dropped_total", "counter", "Usage records dropped because the writer fell behind.")
	p.sample("emx_usage_log_dropped_total", nil, float64(dropped))
	m.mu.Lock()
	unstored := m.unstoredDenials
	m.mu.Unlock()
	p.head("emx_auth_denials_unstored_total", "counter", "Refused-key (401) requests not recorded because more than 60 arrived in a minute.")
	p.sample("emx_auth_denials_unstored_total", nil, float64(unstored))
	p.head("emx_request_body_buffered_bytes", "gauge", "Request bodies held in memory by in-flight requests (bounded by gateway.max_buffered_mb).")
	p.sample("emx_request_body_buffered_bytes", nil, float64(g.buffered.used.Load()))

	db, _ := g.App.DB()
	if teams, err := TeamStatuses(g.App.Config(), db, nil); err == nil {
		p.head("emx_team_budget_used_ratio", "gauge", "Share of a team limit used in its current period (1 = exhausted).")
		for _, t := range teams {
			for _, l := range t.Limits {
				p.sample("emx_team_budget_used_ratio", []string{"team", t.Name, "limit", l.Limit}, l.Ratio)
			}
		}
	}
	if infos, err := KeyInfos(nil); err == nil {
		p.head("emx_key_expiry_seconds", "gauge", "Seconds until an API key expires (negative once expired); keys without expiry are omitted.")
		for _, k := range infos {
			if k.Expires != nil {
				p.sample("emx_key_expiry_seconds", []string{"key", k.Name}, time.Until(*k.Expires).Seconds())
			}
		}
	}
	rs := g.reloadStatus()
	p.head("emx_config_reloads_total", "counter", "Configuration reloads applied.")
	p.sample("emx_config_reloads_total", nil, float64(rs["reloads"].(int64)))
	p.head("emx_config_reload_failures_total", "counter", "Reload attempts refused because config.yaml or models.yaml was invalid.")
	p.sample("emx_config_reload_failures_total", nil, float64(rs["failures"].(int64)))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(p.b.String()))
}
