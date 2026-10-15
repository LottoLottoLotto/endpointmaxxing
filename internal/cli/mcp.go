package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/lotto/substrate/internal/errs"
)

// MCP (Model Context Protocol) over stdio: newline-delimited JSON-RPC 2.0.
// Every command marked tool:true becomes a tool whose arguments mirror its
// flags; calls run the command in --json mode and return its JSON output.

const mcpProtocol = "2025-06-18"

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func cmdMCP() *command {
	return &command{name: "mcp", summary: "serve substrate's commands as MCP tools over stdio (for agents)",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("mcp", pos, 0, 0); err != nil {
					return err
				}
				ctx, stop := signal.NotifyContext(e.ctx, os.Interrupt, syscall.SIGTERM)
				defer stop()
				return serveMCP(ctx, os.Stdin, e.stdout, e.stderr)
			}
		}}
}

// mcpTool is one tool definition.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	argv        func(map[string]any) ([]string, error)
}

func jsonType(t string) map[string]any {
	switch t {
	case "boolean", "integer", "number":
		return map[string]any{"type": t}
	case "string[]":
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	case "duration":
		return map[string]any{"type": "string", "description": "Go duration, e.g. 30s, 10m"}
	}
	return map[string]any{"type": "string"}
}

// tools derives MCP tools from the command schema.
func tools() []mcpTool {
	var out []mcpTool
	for _, s := range schemas() {
		if s.MCPTool == "" {
			continue
		}
		s := s
		props := map[string]any{
			"args": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "positional arguments: " + s.Synopsis},
		}
		types := map[string]string{}
		for _, f := range s.Flags {
			p := jsonType(f.Type)
			desc := f.Usage
			if f.Default != "" && f.Default != "false" && f.Default != "0" {
				desc += " (default " + f.Default + ")"
			}
			p["description"] = desc
			props[f.Name] = p
			types[f.Name] = f.Type
		}
		if s.EngineArgs {
			props["engine_args"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "arguments passed verbatim to the engine (after --)"}
		}
		desc := s.Summary + ". Usage: substrate " + s.Command + " " + s.Synopsis
		out = append(out, mcpTool{
			Name: s.MCPTool, Description: desc,
			InputSchema: map[string]any{"type": "object", "properties": props, "additionalProperties": false},
			argv: func(in map[string]any) ([]string, error) {
				argv := strings.Fields(s.Command)
				keys := make([]string, 0, len(in))
				for k := range in {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				var pos, extra []string
				for _, k := range keys {
					v := in[k]
					switch k {
					case "args", "engine_args":
						list, err := stringList(k, v)
						if err != nil {
							return nil, err
						}
						if k == "args" {
							pos = list
						} else {
							extra = list
						}
						continue
					}
					t, ok := types[k]
					if !ok {
						return nil, fmt.Errorf("unknown argument %q", k)
					}
					if t == "string[]" {
						list, err := stringList(k, v)
						if err != nil {
							return nil, err
						}
						for _, x := range list {
							argv = append(argv, "--"+k+"="+x)
						}
						continue
					}
					argv = append(argv, "--"+k+"="+scalar(v))
				}
				argv = append(argv, pos...)
				if len(extra) > 0 {
					argv = append(append(argv, "--"), extra...)
				}
				return argv, nil
			},
		})
	}
	return out
}

func stringList(k string, v any) ([]string, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", k)
	}
	out := make([]string, len(arr))
	for i, x := range arr {
		out[i] = scalar(x)
	}
	return out, nil
}

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// callTool runs one tool and returns its JSON text and whether it failed.
func callTool(ctx context.Context, t mcpTool, args map[string]any, logw io.Writer) (string, bool) {
	argv, err := t.argv(args)
	if err != nil {
		return jsonText(map[string]string{"error": err.Error(), "code": string(errs.Usage)}), true
	}
	var out bytes.Buffer
	e := &env{ctx: ctx, json: true, stdout: &out, stderr: logw, owner: "mcp"}
	defer e.close()
	err = execute(e, argv)
	if err != nil {
		res := map[string]any{"error": err.Error(), "code": string(errs.CodeOf(err))}
		if out.Len() > 0 {
			res["output"] = json.RawMessage(bytes.TrimSpace(out.Bytes()))
		}
		return jsonText(res), true
	}
	return strings.TrimSpace(out.String()), false
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func serveMCP(ctx context.Context, in io.Reader, out, logw io.Writer) error {
	ts := tools()
	byName := map[string]mcpTool{}
	for _, t := range ts {
		byName[t.Name] = t
	}
	var wmu sync.Mutex
	send := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		out.Write(append(b, '\n'))
		wmu.Unlock()
	}
	reply := func(id json.RawMessage, result any) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	fail := func(id json.RawMessage, code int, msg string) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcErr{code, msg}})
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		for sc.Scan() {
			lines <- append([]byte(nil), sc.Bytes()...)
		}
	}()
	for {
		var line []byte
		select {
		case <-ctx.Done():
			return nil
		case l, ok := <-lines:
			if !ok {
				return sc.Err()
			}
			line = l
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m rpcMsg
		if err := json.Unmarshal(line, &m); err != nil {
			fail(json.RawMessage("null"), -32700, "parse error: "+err.Error())
			continue
		}
		if len(m.ID) == 0 { // notification (initialized, cancelled): nothing to answer
			continue
		}
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(m.Params, &p)
			ver := p.ProtocolVersion
			if ver == "" {
				ver = mcpProtocol
			}
			reply(m.ID, map[string]any{
				"protocolVersion": ver,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "substrate", "version": Version},
				"instructions": "substrate manages local inference engines (llama.cpp, vLLM, SGLang, Strata, ...). " +
					"Typical flow: substrate_model_list or substrate_route, then substrate_ensure to get a ready OpenAI-compatible " +
					"endpoint (api_url), substrate_chat for a quick request, substrate_usage for token accounting. " +
					"Errors carry a stable code: usage, not_found, timeout, engine_failed, conflict, unsupported.",
			})
		case "ping":
			reply(m.ID, map[string]any{})
		case "tools/list":
			reply(m.ID, map[string]any{"tools": ts})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(m.Params, &p); err != nil {
				fail(m.ID, -32602, "invalid params: "+err.Error())
				continue
			}
			t, ok := byName[p.Name]
			if !ok {
				fail(m.ID, -32602, "unknown tool "+p.Name)
				continue
			}
			wg.Add(1)
			go func(id json.RawMessage) { // long calls (ensure) must not block pings or other tools
				defer wg.Done()
				text, isErr := callTool(ctx, t, p.Arguments, logw)
				reply(id, map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr})
			}(m.ID)
		default:
			fail(m.ID, -32601, "method not found: "+m.Method)
		}
	}
}
