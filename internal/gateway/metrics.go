package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/lotto/substrate/internal/gpu"
	"github.com/lotto/substrate/internal/runtime"
	"github.com/lotto/substrate/internal/store"
)

// metrics keeps Prometheus counters and histograms in memory (reset when the gateway restarts,
// which Prometheus rate() handles); the usage database is the durable record.
type metrics struct {
	mu        sync.Mutex
	requests  map[[4]string]int64 // model, engine, api, status
	tokens    map[[3]string]int64 // model, engine, kind
	ttft      map[string]*histogram
	duration  map[string]*histogram
	failovers map[string]int64 // instance
	restarts  map[string]int64 // model
	gpus      []gpu.GPU
	instances []store.InstanceSample
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
		duration: map[string]*histogram{}, failovers: map[string]int64{}, restarts: map[string]int64{}}
}

func (m *metrics) observe(r *store.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[4]string{r.Model, r.Engine, r.API, fmt.Sprint(r.Status)}]++
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

func (m *metrics) failover(in *runtime.Instance) {
	m.mu.Lock()
	m.failovers[in.ID]++
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
	p.head("substrate_requests_total", "counter", "Requests through the gateway.")
	for _, k := range reqKeys {
		p.sample("substrate_requests_total", []string{"model", k[0], "engine", k[1], "api", k[2], "status", k[3]}, float64(m.requests[k]))
	}
	tokKeys := make([][3]string, 0, len(m.tokens))
	for k := range m.tokens {
		tokKeys = append(tokKeys, k)
	}
	sort.Slice(tokKeys, func(i, j int) bool { return fmt.Sprint(tokKeys[i]) < fmt.Sprint(tokKeys[j]) })
	p.head("substrate_tokens_total", "counter", "Tokens reported by engines (kind: prompt, completion, cached, reasoning).")
	for _, k := range tokKeys {
		p.sample("substrate_tokens_total", []string{"model", k[0], "engine", k[1], "kind", k[2]}, float64(m.tokens[k]))
	}
	for _, h := range []struct {
		name, help string
		hs         map[string]*histogram
	}{
		{"substrate_ttft_seconds", "Time to first streamed token.", m.ttft},
		{"substrate_request_duration_seconds", "Request duration, including queueing and autostart.", m.duration},
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
	p.head("substrate_failovers_total", "counter", "Requests moved to another replica after an instance failed.")
	for id, n := range m.failovers {
		p.sample("substrate_failovers_total", []string{"instance", id}, float64(n))
	}
	p.head("substrate_restarts_total", "counter", "Automatic restarts after crashes or failed health checks.")
	for model, n := range m.restarts {
		p.sample("substrate_restarts_total", []string{"model", model}, float64(n))
	}
	gs, samples := m.gpus, m.instances
	m.mu.Unlock()

	p.head("substrate_gpu_memory_used_mib", "gauge", "GPU memory in use (last sample).")
	p.head("substrate_gpu_memory_total_mib", "gauge", "GPU memory size.")
	p.head("substrate_gpu_utilization_ratio", "gauge", "GPU utilization 0-1 (last sample).")
	for _, x := range gs {
		l := []string{"gpu", fmt.Sprint(x.Index), "name", x.Name}
		p.sample("substrate_gpu_memory_used_mib", l, float64(x.MemUsedMiB))
		p.sample("substrate_gpu_memory_total_mib", l, float64(x.MemTotalMiB))
		p.sample("substrate_gpu_utilization_ratio", l, float64(x.UtilPct)/100)
	}
	p.head("substrate_instance_gpu_memory_mib", "gauge", "GPU memory held by an instance's processes (last sample).")
	p.head("substrate_instance_rss_mib", "gauge", "Resident memory of an instance's processes (last sample).")
	for _, s := range samples {
		l := []string{"instance", s.Instance, "engine", s.Engine, "model", s.Model}
		p.sample("substrate_instance_gpu_memory_mib", l, float64(s.GPUMemMiB))
		p.sample("substrate_instance_rss_mib", l, float64(s.RSSMiB))
	}

	ins, _ := runtime.List()
	p.head("substrate_instance_up", "gauge", "1 when the instance is ready to serve.")
	p.head("substrate_instance_inflight_requests", "gauge", "Requests in flight per instance.")
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
		p.sample("substrate_instance_up", l, up)
		p.sample("substrate_instance_inflight_requests", []string{"instance", in.ID, "model", in.Name}, float64(active[in.ID]))
	}
	p.head("substrate_queue_waiting", "gauge", "Requests waiting for a concurrency slot per model.")
	for model, n := range g.lim.queued() {
		p.sample("substrate_queue_waiting", []string{"model", model}, float64(n))
	}
	p.head("substrate_usage_log_dropped_total", "counter", "Usage records dropped because the writer fell behind.")
	p.sample("substrate_usage_log_dropped_total", nil, float64(dropped))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(p.b.String()))
}
