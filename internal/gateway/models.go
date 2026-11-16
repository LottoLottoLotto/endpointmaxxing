package gateway

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/gpu"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/runtime"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/spec"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/tenstorrent"
)

// modelEntry is one model in GET /v1/models. Besides the OpenAI fields it says what a client choosing a
// model wants to know. The fields outside `emx` use the names clients already read; each is absent when
// the gateway does not know it, so a client falls back to its own default instead of a wrong figure.
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// The context window in tokens, under the two names clients look for (vLLM's and OpenRouter's).
	MaxModelLen   int `json:"max_model_len,omitempty"`
	ContextLength int `json:"context_length,omitempty"`
	// Limits.MaxOutputTokens is the most one reply can hold: the gateway's output cap, within the context.
	Limits *modelLimits `json:"limits,omitempty"`
	// Architecture says whether the model takes images (OpenRouter's shape).
	Architecture *modelArchitecture `json:"architecture,omitempty"`
	// EMX holds the rest: instances, engine, apis, load time, measured speed, hardware.
	EMX map[string]any `json:"emx"`
}

type modelLimits struct {
	MaxOutputTokens int `json:"max_output_tokens"`
}

type modelArchitecture struct {
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
}

// setContext records the model's context window; the smallest wins when instances differ.
func (e *modelEntry) setContext(n int) {
	if n > 0 && (e.MaxModelLen == 0 || n < e.MaxModelLen) {
		e.MaxModelLen, e.ContextLength = n, n
		e.EMX["context"] = n
	}
}

// setImages records whether the model takes image input; what a running engine says wins over the files.
func (e *modelEntry) setImages(images bool) {
	in := []string{"text"}
	if images {
		in = append(in, "image")
	}
	e.Architecture = &modelArchitecture{InputModalities: in, OutputModalities: []string{"text"}}
	e.EMX["images"] = images
}

// engineReport is what a running engine said about the model it serves.
type engineReport struct {
	context int
	images  *bool
	slots   int
}

// instanceReport asks instance in's engine, the way its manifest says to, for the model's context window
// and whether it takes images. A complete answer is remembered for the life of the instance.
func (g *Gateway) instanceReport(ctx context.Context, in *runtime.Instance) engineReport {
	key := fmt.Sprintf("%s/%d", in.ID, in.StartTick)
	if r, ok := g.reports.Load(key); ok {
		return r.(engineReport)
	}
	var r engineReport
	e, err := g.App.Engine(in.Engine)
	if err != nil {
		return r
	}
	bodies := map[string][]byte{} // the two questions often share a path
	ask := func(path string) []byte {
		if _, ok := bodies[path]; !ok {
			bodies[path] = runtime.Report(ctx, in, path)
		}
		return bodies[path]
	}
	complete := true
	if c := e.Context; c != nil && c.Path != "" {
		r.context = c.Read(ask(c.Path))
		complete = complete && r.context > 0
	}
	if v := e.Vision; v != nil {
		if images, known := v.Read(ask(v.Path)); known {
			r.images = &images
		} else {
			complete = false
		}
	}
	if sl := e.Slots; sl != nil && sl.Count > 0 {
		r.slots = sl.Count
	} else if sl != nil {
		r.slots = sl.Read(ask(sl.Path))
		complete = complete && r.slots > 0
	}
	if complete { // an engine that did not answer yet is asked again next time
		g.reports.Store(key, r)
	}
	return r
}

// models lists what the caller can use right now: running instances, and registered models that could be
// started (one entry per name). A registered model whose files are gone, or that no installed engine can
// serve, is left out: a client choosing from this list should not be offered a model that cannot answer.
// `emx models` shows those with the reason, and so does this list when asked with ?unavailable=1 (the
// admin UI does, so they can be fixed or removed).
func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	cfg := g.App.Config()
	allows := func(name string) bool { return p.allows(name, cfg) }
	byName := g.modelEntries(r.Context(), allows, r.URL.Query().Get("unavailable") == "1")
	g.addPeerModels(byName, allows) // what the other machines of a mesh serve
	// Admins and auditors also see each model's price (emx price): what `lmx rent publish` offers renters.
	if permits(roleRead, p.role) {
		for name, e := range byName {
			if pr, ok := cfg.Pricing.For(name); ok {
				price := map[string]any{"prompt": pr.Prompt, "completion": pr.Completion, "currency": cfg.Pricing.CurrencyOrDefault(), "per": "1M tokens"}
				if pr.Cached != nil {
					price["cached"] = *pr.Cached
				}
				e.EMX["price"] = price
			}
		}
	}
	// The most one reply can hold: the output cap of the gateway and of the caller's key, within the context.
	keyCap := 0
	if p.key != nil {
		keyCap = p.key.MaxOutputTokens
	}
	limit := outputCap(cfg.Gateway.MaxOutputTokens, keyCap)
	for _, e := range byName {
		out := e.MaxModelLen
		if limit > 0 && (out == 0 || limit < out) {
			out = limit
		}
		if out > 0 {
			e.Limits = &modelLimits{MaxOutputTokens: out}
		}
	}
	for alias, target := range cfg.Aliases {
		t, listed := byName[target]
		if !listed || !allows(target) {
			continue // an alias for a model that is not offered is not offered either
		}
		e := &modelEntry{ID: alias, Object: "model", OwnedBy: "emx", EMX: map[string]any{"alias_for": target}}
		byName[alias] = e
		e.setContext(t.MaxModelLen)
		e.Limits, e.Architecture = t.Limits, t.Architecture
		for _, k := range []string{"apis", "images", "load_seconds", "decode_tok_per_sec", "hardware", "concurrency", "nodes"} {
			if v, ok := t.EMX[k]; ok {
				e.EMX[k] = v
			}
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	data := make([]*modelEntry, len(names))
	for i, n := range names {
		data[i] = byName[n]
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// modelEntries lists the models of this machine that allows lets through: running instances, and registered
// models that could be started (with withUnavailable, also those that could not), with what is known of each.
func (g *Gateway) modelEntries(ctx context.Context, allows func(string) bool, withUnavailable bool) map[string]*modelEntry {
	byName := map[string]*modelEntry{}
	entry := func(name string, created int64) *modelEntry {
		e := byName[name]
		if e == nil {
			e = &modelEntry{ID: name, Object: "model", Created: created, OwnedBy: "emx",
				EMX: map[string]any{"instances": []map[string]any{}}}
			byName[name] = e
		}
		return e
	}
	addAPIs := func(e *modelEntry, apis []string) {
		have, _ := e.EMX["apis"].([]string)
		for _, a := range apis {
			if !slices.Contains(have, a) {
				have = append(have, a)
			}
		}
		if len(have) > 0 {
			e.EMX["apis"] = have
		}
	}
	parallel := map[string]int{} // requests each model's engine works on at once, where known
	active := map[string]int{}   // requests in flight per model
	ins, _ := runtime.List()
	for _, in := range ins {
		if !in.Alive() {
			continue
		}
		g.activeMu.Lock()
		busy := g.active[in.ID]
		g.activeMu.Unlock()
		names := []string{in.Name}
		if in.Hosting { // a multi-model host is listed under the models it holds
			names = g.hosted(ctx, in)
		}
		for _, n := range names {
			if !allows(n) {
				continue
			}
			e := entry(n, in.StartedAt.Unix())
			status := g.instanceStatus(ctx, in)
			e.EMX["instances"] = append(e.EMX["instances"].([]map[string]any),
				map[string]any{"id": in.ID, "engine": in.Engine, "status": status, "apis": in.APIs, "gpus": in.GPUs})
			addAPIs(e, in.APIs)
			hw := map[string]any{"kind": "gpu", "loaded": true}
			if in.Accelerator == spec.AccelTenstorrent {
				hw["kind"], hw["chips"] = spec.AccelTenstorrent, in.Devices
			} else if len(in.GPUs) > 0 {
				hw["gpus"] = in.GPUs
			}
			e.EMX["hardware"] = hw
			if !in.Hosting {
				active[n] += busy
			}
			if status == runtime.StatusReady && !in.Hosting {
				rep := g.instanceReport(ctx, in)
				e.setContext(rep.context)
				if rep.images != nil {
					e.setImages(*rep.images)
				}
				parallel[n] += rep.slots // replicas add up
			}
		}
	}
	g.Reload() // see models added since; an invalid config keeps the running one (logged)
	free := g.freeHardware()
	for _, m := range g.App.RegisteredModels() {
		if !allows(m.Name) {
			continue
		}
		offer := g.App.Offer(m)
		_, running := byName[m.Name]
		if !running && offer.Unavailable != "" && !withUnavailable {
			continue // could not be started: its files are gone, or no installed engine serves it
		}
		e := entry(m.Name, 0)
		if !running && offer.Unavailable != "" {
			e.EMX["unavailable"] = offer.Unavailable
		}
		e.EMX["registered"] = true
		e.EMX["format"] = m.Format
		if m.Engine != "" {
			e.EMX["engine"] = m.Engine
		}
		if e.MaxModelLen == 0 { // not running: what it is registered to start with
			e.setContext(offer.Context)
		}
		if e.Architecture == nil && offer.Images != nil {
			e.setImages(*offer.Images)
		}
		if parallel[m.Name] == 0 { // not running, or an engine that does not say: what it is registered with
			parallel[m.Name] = offer.Parallel
		}
		if !running {
			addAPIs(e, offer.APIs)
			if hw := free.forOffer(offer.Accelerator, offer.Chips, offer.NeedMiB); hw != nil {
				e.EMX["hardware"] = hw
			}
		}
	}
	queued := g.lim.queued()
	for name, e := range byName {
		e.EMX["concurrency"] = concurrency(parallel[name], g.maxConcurrency(name), active[name], queued[name])
	}
	g.addHistory(byName)
	if node := g.App.Config().Mesh; node.Enabled() {
		for _, e := range byName {
			e.EMX["nodes"] = []string{node.Node}
		}
	}
	return byName
}

// concurrency tells a client how many others it shares a model with. parallel is how many requests the
// engine works on at the same time (0: not known), limit how many the gateway lets through (0: no limit).
//
//	mode      "single" when one request runs at a time (each gets the engine's full speed), "shared" when
//	          several do (more total throughput, less for each); absent when neither number is known
//	serves    requests that run at the same time before others wait
//	parallel  the engine's slots
//	limit     the gateway's cap for the model
//	active    requests running now
//	queued    requests waiting for a slot now
func concurrency(parallel, limit, active, queued int) map[string]any {
	c := map[string]any{"active": active, "queued": queued}
	serves := parallel
	if limit > 0 && (serves == 0 || limit < serves) {
		serves = limit
	}
	if parallel > 0 {
		c["parallel"] = parallel
	}
	if limit > 0 {
		c["limit"] = limit
	}
	if serves > 0 {
		c["serves"] = serves
		c["mode"] = "shared"
		if serves == 1 {
			c["mode"] = "single"
		}
	}
	return c
}

// historyWindow is how far back measured speed is taken from.
const historyWindow = 30 * 24 * time.Hour

// addHistory adds what the usage database knows about each model: how long its last load took and the
// decode speed measured over real requests in the last 30 days.
func (g *Gateway) addHistory(byName map[string]*modelEntry) {
	if !g.App.Config().Logging.Enabled {
		return
	}
	db, err := g.App.DB()
	if err != nil {
		return
	}
	if rows, err := db.Usage("model", store.Filter{Since: time.Now().Add(-historyWindow)}); err == nil {
		for _, row := range rows {
			if e := byName[row.Key]; e != nil && row.DecodeTokPerSec != nil && *row.DecodeTokPerSec > 0 {
				e.EMX["decode_tok_per_sec"] = float64(int(*row.DecodeTokPerSec*10+0.5)) / 10
			}
		}
	}
	for name, e := range byName {
		if d, err := db.LastLoad(name); err == nil && d > 0 {
			e.EMX["load_seconds"] = float64(int(d.Seconds()*10+0.5)) / 10
		}
	}
}

// freeHardware is what is free on the host right now: memory per GPU, and Tenstorrent chips no job holds.
type freeHardware struct {
	gpuFreeMiB int64 // the roomiest GPU
	gpus       int
	chipsFree  int
}

func (g *Gateway) freeHardware() freeHardware {
	var f freeHardware
	gs := g.metrics.gpuSnapshot()
	if len(gs) == 0 { // sampling is off, or has not run yet
		gs, _ = gpu.List()
	}
	f.gpus = len(gs)
	for _, card := range gs {
		f.gpuFreeMiB = max(f.gpuFreeMiB, card.MemTotalMiB-card.MemUsedMiB)
	}
	for _, n := range tenstorrent.Chips() {
		if !runtime.TT.Held(n) && len(tenstorrent.Users(n)) == 0 {
			f.chipsFree++
		}
	}
	return f
}

// forOffer describes the hardware a stopped model needs and whether it is free now. `free_now` false does
// not mean a request fails: the gateway may make room by stopping an idle model it started, and waits for
// a busy chip.
func (f freeHardware) forOffer(accelerator string, chips int, needMiB int64) map[string]any {
	switch accelerator {
	case spec.AccelTenstorrent:
		return map[string]any{"kind": spec.AccelTenstorrent, "chips": chips, "free_now": f.chipsFree >= chips}
	case "gpu":
		if f.gpus == 0 {
			return nil // a host without GPUs: the engine runs on the CPU
		}
		hw := map[string]any{"kind": "gpu", "free_mib": f.gpuFreeMiB}
		if needMiB > 0 {
			hw["need_mib"], hw["free_now"] = needMiB, needMiB <= f.gpuFreeMiB
		}
		return hw
	}
	return nil
}
