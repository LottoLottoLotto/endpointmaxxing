package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/gateway"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
)

// The mesh commands ask the gateway running on this machine (they need its admin key in $EMX_API_KEY); it
// answers for itself and relays to the other machines (?node=).

func cmdMesh() *command {
	return &command{name: "mesh", summary: "the machines of this gateway's mesh: status, instances, starting and stopping models anywhere, usage of all",
		sub: []*command{meshStatusCmd(), meshPsCmd(), meshRunCmd(), meshStopCmd(), meshUsageCmd()}}
}

// gatewayJSON calls the local gateway's API and decodes its answer into out.
func gatewayJSON(e *env, method, path string, body, out any) error {
	a, err := e.app()
	if err != nil {
		return err
	}
	base, client := gateway.Client(&a.Config().Gateway)
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequestWithContext(e.ctx, method, base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key := os.Getenv("EMX_API_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return errs.New(errs.NotFound, "no gateway answering at %s: %v", base, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var eb struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(data, &eb)
		code := errs.Internal
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			code = errs.Forbidden
		case http.StatusNotFound:
			code = errs.NotFound
		case http.StatusBadRequest:
			code = errs.Usage
		case http.StatusConflict:
			code = errs.Conflict
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			code = errs.Capacity
		}
		return errs.New(code, "%s %s: %s: %s", method, path, resp.Status, eb.Error.Message)
	}
	return json.Unmarshal(data, out)
}

type meshView struct {
	Enabled      bool       `json:"enabled"`
	Node         string     `json:"node"`
	KeysFrom     string     `json:"keys_from"`
	Follower     bool       `json:"follower"`
	KeysCopiedAt *time.Time `json:"keys_copied_at"`
	KeysCopyErr  string     `json:"keys_copy_error"`
	Models       []string   `json:"models"`
	Peers        []struct {
		Name     string     `json:"name"`
		URL      string     `json:"url"`
		Up       bool       `json:"up"`
		LastSeen *time.Time `json:"last_seen"`
		Error    string     `json:"error"`
		RTTMS    int64      `json:"rtt_ms"`
		Version  string     `json:"version"`
		Models   []string   `json:"models"`
	} `json:"peers"`
}

func meshOf(e *env) (*meshView, error) {
	var v meshView
	if err := gatewayJSON(e, http.MethodGet, "/emx/v1/mesh", nil, &v); err != nil {
		return nil, err
	}
	if !v.Enabled {
		return nil, errs.New(errs.Usage, "this gateway is not in a mesh (no mesh.peers in config.yaml)")
	}
	return &v, nil
}

// nodes is every machine of the mesh: this one first, then the peers that answer.
func (v *meshView) nodes(upOnly bool) []string {
	out := []string{v.Node}
	for _, p := range v.Peers {
		if p.Up || !upOnly {
			out = append(out, p.Name)
		}
	}
	return out
}

func nodeQuery(v *meshView, node string) string {
	if node == v.Node {
		return ""
	}
	return "node=" + url.QueryEscape(node)
}

func withQuery(path, q string) string {
	if q == "" {
		return path
	}
	if strings.Contains(path, "?") {
		return path + "&" + q
	}
	return path + "?" + q
}

func meshStatusCmd() *command {
	return &command{name: "status", summary: "which machines answer, how fast, what each serves, where keys come from", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("mesh status", pos, 0, 0); err != nil {
					return err
				}
				v, err := meshOf(e)
				if err != nil {
					return err
				}
				return e.out(v, func(w io.Writer) {
					keys := v.KeysFrom + " (this machine)"
					if v.Follower {
						keys = "copied from " + v.KeysFrom
						switch {
						case v.KeysCopyErr != "":
							keys += ", last copy failed: " + v.KeysCopyErr
						case v.KeysCopiedAt == nil:
							keys += ", not copied yet"
						}
					}
					fmt.Fprintf(w, "this machine: %s; keys, teams, prices and aliases: %s\n\n", v.Node, keys)
					t := [][]string{{v.Node, "this machine", "-", Version, dash(strings.Join(v.Models, ", "))}}
					for _, p := range v.Peers {
						status, rtt := "up", fmt.Sprintf("%dms", p.RTTMS)
						if !p.Up {
							status, rtt = "DOWN: "+p.Error, "-"
						}
						t = append(t, []string{p.Name, status, rtt, dash(p.Version), dash(strings.Join(p.Models, ", "))})
					}
					e.table("NODE\tSTATUS\tRTT\tVERSION\tMODELS", t)
				})
			}
		}}
}

type nodeInstance struct {
	Node     string         `json:"node"`
	Instance map[string]any `json:"instance"`
	Status   string         `json:"status"`
	Active   int            `json:"active_requests"`
}

func meshPsCmd() *command {
	return &command{name: "ps", summary: "running instances on every machine of the mesh", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("mesh ps", pos, 0, 0); err != nil {
					return err
				}
				v, err := meshOf(e)
				if err != nil {
					return err
				}
				var all []nodeInstance
				var failed []string
				for _, n := range v.nodes(true) {
					var list []nodeInstance
					if err := gatewayJSON(e, http.MethodGet, withQuery("/emx/v1/instances", nodeQuery(v, n)), nil, &list); err != nil {
						failed = append(failed, fmt.Sprintf("%s: %v", n, err))
						continue
					}
					for _, in := range list {
						in.Node = n
						all = append(all, in)
					}
				}
				return e.out(map[string]any{"instances": all, "unreachable": failed}, func(w io.Writer) {
					t := make([][]string, 0, len(all))
					for _, in := range all {
						t = append(t, []string{in.Node, fmt.Sprint(in.Instance["id"]), fmt.Sprint(in.Instance["engine"]), in.Status,
							fmt.Sprint(in.Active), fmt.Sprint(in.Instance["owner"])})
					}
					if len(t) == 0 {
						fmt.Fprintln(w, "no instances on any machine")
					} else {
						e.table("NODE\tINSTANCE\tENGINE\tSTATUS\tACTIVE\tOWNER", t)
					}
					for _, f := range failed {
						fmt.Fprintln(w, "unreachable:", f)
					}
				})
			}
		}}
}

func meshRunCmd() *command {
	return &command{name: "run", synopsis: "<node> <model> [--timeout 10m]", summary: "start a model on a machine of the mesh (waits until it is ready)", tool: true,
		build: func(fs *flag.FlagSet) action {
			timeout := fs.String("timeout", "", "longest wait for the model to be ready (default: the engine's)")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("mesh run", pos, 2, 2); err != nil {
					return err
				}
				v, err := meshOf(e)
				if err != nil {
					return err
				}
				var out map[string]any
				body := map[string]any{"model": pos[1]}
				if *timeout != "" {
					body["timeout"] = *timeout
				}
				if err := gatewayJSON(e, http.MethodPost, withQuery("/emx/v1/ensure", nodeQuery(v, pos[0])), body, &out); err != nil {
					return err
				}
				out["node"] = pos[0]
				in, _ := out["instance"].(map[string]any)
				return e.out(out, func(w io.Writer) { fmt.Fprintf(w, "%s: %v ready\n", pos[0], in["id"]) })
			}
		}}
}

func meshStopCmd() *command {
	return &command{name: "stop", synopsis: "<node> <instance-or-model>", summary: "stop an instance on a machine of the mesh (after its requests finish)", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("mesh stop", pos, 2, 2); err != nil {
					return err
				}
				v, err := meshOf(e)
				if err != nil {
					return err
				}
				var out map[string]any
				if err := gatewayJSON(e, http.MethodPost, withQuery("/emx/v1/stop", nodeQuery(v, pos[0])), map[string]any{"id": pos[1]}, &out); err != nil {
					return err
				}
				out["node"] = pos[0]
				return e.out(out, func(w io.Writer) { fmt.Fprintf(w, "%s: stopped %v\n", pos[0], out["stopped"]) })
			}
		}}
}

func meshUsageCmd() *command {
	return &command{name: "usage", synopsis: "[--by model|client|team|node|...] [--since 24h]",
		summary: "usage of the whole mesh: what clients sent to each machine, added up (each request once)", tool: true,
		build: func(fs *flag.FlagSet) action {
			by := fs.String("by", "model", "group by (as emx usage --by), or total")
			since := fs.String("since", "24h", "window start: a duration back (24h) or a time")
			perNode := fs.Bool("per-node", false, "also show each machine's own rows")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("mesh usage", pos, 0, 0); err != nil {
					return err
				}
				v, err := meshOf(e)
				if err != nil {
					return err
				}
				q := url.Values{"since": {*since}, "entered": {"1"}}
				if *by != "total" {
					q.Set("by", *by)
				}
				sum := map[string]*store.UsageRow{}
				per := map[string][]store.UsageRow{}
				var failed []string
				for _, n := range v.nodes(true) {
					var rows []store.UsageRow
					if err := gatewayJSON(e, http.MethodGet, withQuery("/emx/v1/usage?"+q.Encode(), nodeQuery(v, n)), nil, &rows); err != nil {
						failed = append(failed, fmt.Sprintf("%s: %v", n, err))
						continue
					}
					per[n] = rows
					for _, r := range rows {
						addUsage(sum, r)
					}
				}
				keys := make([]string, 0, len(sum))
				for k := range sum {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				total := make([]store.UsageRow, len(keys))
				for i, k := range keys {
					total[i] = *sum[k]
				}
				return e.out(map[string]any{"by": *by, "since": *since, "rows": total, "per_node": per, "unreachable": failed}, func(w io.Writer) {
					show := func(title string, rows []store.UsageRow) {
						fmt.Fprintln(w, title)
						t := make([][]string, len(rows))
						for i, r := range rows {
							t[i] = []string{dash(r.Key), fmt.Sprint(r.Requests), fmt.Sprint(r.Errors), fmt.Sprint(r.PromptTokens),
								fmt.Sprint(r.CompletionTokens), optf(r.AvgTTFTMS, "ms"), optf(r.DecodeTokPerSec, "")}
						}
						e.table(strings.ToUpper(*by)+"\tREQS\tERRS\tPROMPT\tCOMPLETION\tAVG TTFT\tDECODE TOK/S", t)
						fmt.Fprintln(w)
					}
					show(fmt.Sprintf("whole mesh (%d machines), since %s:", len(per), *since), total)
					if *perNode {
						for _, n := range v.nodes(true) {
							if rows, ok := per[n]; ok {
								show(n+":", rows)
							}
						}
					}
					for _, f := range failed {
						fmt.Fprintln(w, "unreachable (not counted):", f)
					}
				})
			}
		}}
}

// addUsage adds row r into sum: counts add up, averages are weighted by requests.
func addUsage(sum map[string]*store.UsageRow, r store.UsageRow) {
	s := sum[r.Key]
	if s == nil {
		c := r
		sum[r.Key] = &c
		return
	}
	weighted := func(a *float64, na int64, b *float64, nb int64) *float64 {
		switch {
		case a == nil:
			return b
		case b == nil:
			return a
		}
		v := (*a*float64(na) + *b*float64(nb)) / float64(max(na+nb, 1))
		return &v
	}
	s.AvgTTFTMS = weighted(s.AvgTTFTMS, s.Requests, r.AvgTTFTMS, r.Requests)
	s.AvgDurationMS = weighted(s.AvgDurationMS, s.Requests, r.AvgDurationMS, r.Requests)
	s.DecodeTokPerSec = weighted(s.DecodeTokPerSec, s.CompletionTokens, r.DecodeTokPerSec, r.CompletionTokens)
	s.Requests += r.Requests
	s.Errors += r.Errors
	s.Flagged += r.Flagged
	s.Blocked += r.Blocked
	s.PromptTokens += r.PromptTokens
	s.CompletionTokens += r.CompletionTokens
	s.CachedTokens += r.CachedTokens
	s.ReasoningTokens += r.ReasoningTokens
	s.UnmeteredReqs += r.UnmeteredReqs
	s.Models = max(s.Models, r.Models)
	s.Clients = max(s.Clients, r.Clients)
	if r.Cost != nil {
		c := *r.Cost
		if s.Cost != nil {
			c += *s.Cost
		}
		s.Cost = &c
	}
}
