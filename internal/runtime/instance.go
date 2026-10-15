package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/gpu"
	"github.com/lotto/substrate/internal/lock"
	"github.com/lotto/substrate/internal/paths"
	"github.com/lotto/substrate/internal/spec"
	"github.com/lotto/substrate/internal/store"
	"github.com/lotto/substrate/internal/tenstorrent"
)

// Instance is a launched engine process, persisted as state/instances/<id>.json (mode 0600: it may hold an API key).
type Instance struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Engine    string            `json:"engine"`
	Model     string            `json:"model"`
	Format    string            `json:"format"`
	PID       int               `json:"pid"`
	StartTick uint64            `json:"start_tick"` // /proc/<pid>/stat starttime; guards against PID reuse
	Host      string            `json:"host"`
	Port      int               `json:"port"`
	BaseURL   string            `json:"base_url"`
	HealthURL string            `json:"health_url"`
	APIs      []string          `json:"apis"`
	Argv      []string          `json:"argv"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Log       string            `json:"log"`
	APIKey    string            `json:"api_key,omitempty"` // the key the engine requires (unified --api-key)
	Owner     string            `json:"owner"`             // "cli", "gateway" or "mcp"
	StartedAt time.Time         `json:"started_at"`
	ReadyAt   *time.Time        `json:"ready_at,omitempty"`
	// ExitNoted is set once the instance's exit has been recorded as an event.
	ExitNoted bool `json:"exit_noted,omitempty"`
	// GPUs and PlannedMiB (per GPU) record the placement decision; a loading instance
	// reserves PlannedMiB minus what it already uses, so concurrent launches don't overcommit.
	GPUs       []int `json:"gpus,omitempty"`
	PlannedMiB int64 `json:"planned_mib,omitempty"`
	// Accelerator is the device kind; Devices the Tenstorrent chips the instance owns.
	Accelerator string `json:"accelerator,omitempty"`
	Devices     []int  `json:"devices,omitempty"`
	// Hosting marks a multi-model host: it serves the models it has loaded (see HostedModels), not Name.
	Hosting bool `json:"hosting,omitempty"`
	// Cleanup runs after the instance stops or dies; ResetOnKill resets its chips after a forced stop or crash.
	Cleanup     [][]string `json:"cleanup,omitempty"`
	ResetOnKill bool       `json:"reset_on_kill,omitempty"`

	StopSignal  string        `json:"stop_signal,omitempty"`
	StopTimeout time.Duration `json:"stop_timeout"`
	ReadyIn     time.Duration `json:"ready_timeout"`
}

// Status of an instance.
const (
	StatusReady    = "ready"
	StatusStarting = "starting"
	StatusExited   = "exited"
)

// Recorder receives lifecycle events; nil disables recording. Set once at startup.
var Recorder func(store.Event)

// TT locates Tenstorrent chip locks and tt-smi. Set once at startup.
var TT = tenstorrent.Config{Lock: "/var/tmp/tenstorrent/chip-{n}.lock", TTSmi: "tt-smi"}

func record(kind string, in *Instance, dur *time.Duration, detail string) {
	if Recorder == nil {
		return
	}
	e := store.Event{Kind: kind, Instance: in.ID, Engine: in.Engine, Model: in.Name, Owner: in.Owner, Detail: detail}
	if in.PID > 0 {
		p := int64(in.PID)
		e.PID = &p
	}
	if in.Port > 0 {
		p := int64(in.Port)
		e.Port = &p
	}
	if dur != nil {
		ms := dur.Milliseconds()
		e.DurationMS = &ms
	}
	Recorder(e)
}

// RecordEvent records a lifecycle event for in (unhealthy, healthy, restart, ...).
func RecordEvent(kind string, in *Instance, detail string) { record(kind, in, nil, detail) }

func instancesDir() string { return filepath.Join(paths.State(), "instances") }
func logsDir() string      { return filepath.Join(paths.State(), "logs") }

// LogPath is where instance id logs (kept after the instance stops).
func LogPath(id string) string { return filepath.Join(logsDir(), id+".log") }

func (in *Instance) path() string { return filepath.Join(instancesDir(), in.ID+".json") }
func lockFor(id string) string    { return filepath.Join(instancesDir(), "."+id+".lock") }

// HasAPI reports whether the instance's engine serves api.
func (in *Instance) HasAPI(api string) bool {
	for _, a := range in.APIs {
		if a == api {
			return true
		}
	}
	return false
}

// Redacted returns a copy safe to print: the API key is removed from the record, argv and env.
func (in *Instance) Redacted() *Instance {
	c := *in
	c.APIKey = ""
	c.Argv = Redact(in.Argv, in.APIKey)
	if in.APIKey != "" && len(in.Env) > 0 {
		c.Env = map[string]string{}
		for k, v := range in.Env {
			c.Env[k] = strings.ReplaceAll(v, in.APIKey, "***")
		}
	}
	return &c
}

// Redact replaces every occurrence of secret in argv.
func Redact(argv []string, secret string) []string {
	if secret == "" {
		return argv
	}
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = strings.ReplaceAll(a, secret, "***")
	}
	return out
}

func (in *Instance) save() error {
	if err := os.MkdirAll(instancesDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", in.path(), os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, in.path())
}

// List returns every recorded instance, sorted by ID.
func List() ([]*Instance, error) {
	files, err := filepath.Glob(filepath.Join(instancesDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Instance
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var in Instance
		if json.Unmarshal(data, &in) == nil {
			out = append(out, &in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get returns the instance with id.
func Get(id string) (*Instance, error) {
	data, err := os.ReadFile(filepath.Join(instancesDir(), id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errs.New(errs.NotFound, "no instance %q (see `substrate ps`)", id)
	}
	if err != nil {
		return nil, err
	}
	var in Instance
	return &in, json.Unmarshal(data, &in)
}

// Find resolves an instance by ID, or by served name when exactly one matches.
func Find(ref string) (*Instance, error) {
	if in, err := Get(ref); err == nil {
		return in, nil
	}
	all, err := List()
	if err != nil {
		return nil, err
	}
	var hits []*Instance
	for _, in := range all {
		if in.Name == ref {
			hits = append(hits, in)
		}
	}
	switch len(hits) {
	case 0:
		return nil, errs.New(errs.NotFound, "no instance %q (see `substrate ps`)", ref)
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	return nil, errs.New(errs.Conflict, "%q is served by several instances (%s); give the id", ref, strings.Join(ids, ", "))
}

// stat reads /proc/<pid>/stat fields after the command name (field 3 onward); nil when gone.
func stat(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(s[i+1:])
}

// procStart reads the process start time in clock ticks; 0 when the process is gone or a zombie.
func procStart(pid int) uint64 {
	f := stat(pid)
	if len(f) < 20 || f[0] == "Z" {
		return 0
	}
	t, _ := strconv.ParseUint(f[19], 10, 64)
	return t
}

// Alive reports whether the instance's process still runs.
func (in *Instance) Alive() bool {
	return in.PID > 0 && in.StartTick != 0 && procStart(in.PID) == in.StartTick
}

var probeClient = &http.Client{Timeout: 3 * time.Second}

// Healthy probes the health endpoint once.
func (in *Instance) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.HealthURL, nil)
	if err != nil {
		return false
	}
	if in.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+in.APIKey)
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Status is ready, starting or exited.
func (in *Instance) Status(ctx context.Context) string {
	if !in.Alive() {
		return StatusExited
	}
	if in.Healthy(ctx) {
		return StatusReady
	}
	return StatusStarting
}

// FreePort returns a free TCP port on host.
func FreePort(host string) (int, error) {
	if host == "0.0.0.0" || host == "" || host == "::" {
		host = "127.0.0.1"
	}
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// PortInUse reports whether something listens on host:port.
func PortInUse(host string, port int) bool {
	if host == "0.0.0.0" || host == "" || host == "::" {
		host = "127.0.0.1"
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// InstanceID is the default instance id for a model on an engine.
func InstanceID(name, engine string) string { return name + "." + engine }

// ErrPortInUse marks a start refused because its port is taken (callers with an auto port retry).
var ErrPortInUse = errors.New("port already in use")

// rotateLog keeps the previous three logs of an instance as .1 .. .3.
func rotateLog(path string) {
	for i := 3; i > 0; i-- {
		src := path
		if i > 1 {
			src = fmt.Sprintf("%s.%d", path, i-1)
		}
		os.Rename(src, fmt.Sprintf("%s.%d", path, i))
	}
}

// Start launches plan in its own session, logging to state/logs/<id>.log.
// The instance lock makes concurrent starts of one id (CLI and gateway) safe.
func Start(p *Plan, id, owner string) (*Instance, error) {
	var in *Instance
	err := lock.With(lockFor(id), func() error {
		var err error
		in, err = start(p, id, owner)
		return err
	})
	if err != nil {
		return nil, err
	}
	record("start", in, nil, ShellJoin(Redact(in.Argv, in.APIKey)))
	return in, nil
}

func start(p *Plan, id, owner string) (*Instance, error) {
	if old, err := Get(id); err == nil {
		if old.Alive() {
			return nil, errs.New(errs.Conflict, "instance %s is already running (pid %d, %s)", id, old.PID, old.BaseURL)
		}
		old.noteExit()
		os.Remove(old.path())
	}
	// A container or process left behind by an earlier run of this id would hold the name or the chips.
	runCleanup(p.Cleanup, nil)
	if p.Assets != "" {
		if err := spec.SyncAssets(p.Engine, p.Assets); err != nil {
			return nil, errs.New(errs.EngineFailed, "writing %s's files to %s: %v", p.Engine, p.Assets, err)
		}
	}
	for _, c := range p.Prepare {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			return nil, errs.New(errs.EngineFailed, "prepare %s: %v: %s", ShellJoin(c), err, strings.TrimSpace(string(out)))
		}
	}
	if PortInUse(p.Host, p.Port) {
		return nil, errs.Wrap(errs.Conflict, fmt.Errorf("port %d: %w", p.Port, ErrPortInUse))
	}
	var lockFiles []*os.File
	if p.Accelerator == spec.AccelTenstorrent && len(p.Devices) > 0 {
		var err error
		if p.lockMode == "engine" {
			err = TT.Check(p.Devices) // the engine takes the locks itself
		} else {
			lockFiles, err = TT.Acquire(p.Devices)
		}
		if err != nil {
			return nil, errs.Wrap(errs.Capacity, err)
		}
		// The engine inherits the lock descriptors: the locks live exactly as long as it does.
		defer func() {
			for _, f := range lockFiles {
				f.Close()
			}
		}()
	}
	if err := os.MkdirAll(logsDir(), 0o755); err != nil {
		return nil, err
	}
	logPath := LogPath(id)
	rotateLog(logPath)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "# substrate %s: %s\n# cwd: %s\n", time.Now().Format(time.RFC3339), ShellJoin(Redact(p.Argv, p.apiKey)), p.Cwd)
	for _, k := range sortedKeys(p.Env) {
		if k != "PATH" {
			fmt.Fprintf(logf, "# env: %s=%s\n", k, Redact([]string{p.Env[k]}, p.apiKey)[0])
		}
	}

	cmd := exec.Command(p.Argv[0], p.Argv[1:]...)
	cmd.Dir = p.Cwd
	cmd.Env = os.Environ()
	for _, k := range sortedKeys(p.Env) {
		cmd.Env = append(cmd.Env, k+"="+p.Env[k])
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.ExtraFiles = lockFiles
	// Own session: the engine outlives this CLI and is stopped as a process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, errs.New(errs.EngineFailed, "start %s: %w", p.Argv[0], err)
	}
	go cmd.Wait() // reap if this process lives on (gateway, MCP)

	in := &Instance{
		ID: id, Name: p.Name, Engine: p.Engine, Model: p.Model, Format: p.Format,
		PID: cmd.Process.Pid, Host: p.Host, Port: p.Port, BaseURL: p.BaseURL, HealthURL: p.HealthURL,
		APIs: p.APIs, Argv: p.Argv, Env: p.Env, Cwd: p.Cwd, Log: logPath, Owner: owner,
		StartedAt: time.Now().UTC(), StopSignal: p.stopSignal, StopTimeout: p.stopTimeout, ReadyIn: p.readyIn,
		APIKey: p.apiKey, GPUs: p.GPUs, PlannedMiB: p.PlannedMiB,
		Accelerator: p.Accelerator, Devices: p.Devices, Cleanup: p.Cleanup, ResetOnKill: p.resetOnKill,
		Hosting: p.Hosting,
	}
	for i := 0; i < 50 && in.StartTick == 0; i++ {
		if in.StartTick = procStart(in.PID); in.StartTick == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if in.StartTick == 0 {
		return nil, errs.New(errs.EngineFailed, "%s exited immediately; log: %s\n%s", p.Engine, logPath, Tail(logPath, 20))
	}
	return in, in.save()
}

// WaitReady polls health until ready, the process exits, or the timeout passes.
func (in *Instance) WaitReady(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = in.ReadyIn
	}
	deadline := time.Now().Add(timeout)
	for {
		if !in.Alive() {
			return errs.New(errs.EngineFailed, "%s exited during startup; log: %s\n%s", in.ID, in.Log, Tail(in.Log, 30))
		}
		if in.Healthy(ctx) {
			return nil
		}
		if time.Now().After(deadline) {
			return errs.New(errs.Timeout, "%s not ready after %s; log: %s", in.ID, timeout, in.Log)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// AwaitStart is WaitReady for the process that launched the instance: it
// records load time (ready) or the failure (start_failed) and stores ReadyAt.
// A failed start is stopped so it does not hold the GPU.
func (in *Instance) AwaitStart(ctx context.Context, timeout time.Duration) error {
	err := in.WaitReady(ctx, timeout)
	took := time.Since(in.StartedAt)
	if err == nil {
		now := time.Now().UTC()
		in.ReadyAt = &now
		lock.With(lockFor(in.ID), func() error {
			if cur, gerr := Get(in.ID); gerr == nil && cur.PID == in.PID {
				return in.save()
			}
			return nil
		})
		record("ready", in, &took, "")
		return nil
	}
	if ctx.Err() != nil {
		return err // the caller gave up waiting; the engine keeps loading
	}
	record("start_failed", in, &took, err.Error())
	in.ExitNoted = true
	in.Stop()
	return err
}

// NoteExit records an exited instance's exit once (ps, the gateway's monitor).
func (in *Instance) NoteExit() {
	lock.With(lockFor(in.ID), func() error {
		cur, err := Get(in.ID)
		if err != nil || cur.PID != in.PID || cur.ExitNoted || cur.Alive() {
			return nil
		}
		cur.noteExit()
		*in = *cur
		return nil
	})
}

func (in *Instance) noteExit() {
	if in.ExitNoted {
		return
	}
	up := time.Since(in.StartedAt)
	detail := Tail(in.Log, 5)
	// The leader is gone, but its workers (vLLM EngineCore, SGLang scheduler) may still hold
	// the GPU. Skip when the pid was reused: that process leads somebody else's session.
	if procStart(in.PID) == 0 {
		if orphans := SessionPIDs(in.PID); len(orphans) > 0 {
			killSession(in.PID)
			detail = fmt.Sprintf("killed %d orphaned worker(s)\n%s", len(orphans), detail)
		}
	}
	runCleanup(in.Cleanup, in)
	// Reset a crashed engine's chips only once they are free: never under another job.
	if in.waitChipsFree(time.Minute) && in.ResetOnKill {
		detail = in.reset("crashed") + detail
	}
	record("exited", in, &up, detail)
	in.ExitNoted = true
	if _, err := os.Stat(in.path()); err == nil {
		in.save()
	}
}

// Stop signals the process group, escalates to SIGKILL after the stop timeout,
// records the stop and removes the instance record.
func (in *Instance) Stop() error { return in.StopFor("") }

// StopFor is Stop with a reason recorded on the stop event (evicted, idle, unhealthy).
func (in *Instance) StopFor(reason string) error {
	return lock.With(lockFor(in.ID), func() error { return in.stop(reason) })
}

func (in *Instance) stop(reason string) error {
	if in.Alive() {
		sig := syscall.SIGTERM
		if s, ok := spec.Signals[in.StopSignal]; ok {
			sig = s
		}
		// Setsid made the engine a group leader: pgid == pid.
		if err := syscall.Kill(-in.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		timeout := in.StopTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		deadline := time.Now().Add(timeout)
		for in.Alive() && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		forced := in.Alive()
		if forced {
			syscall.Kill(-in.PID, syscall.SIGKILL)
			for i := 0; i < 50 && in.Alive(); i++ {
				time.Sleep(100 * time.Millisecond)
			}
		}
		// Children that left the group (or a group that outlived its leader) still hold the GPU.
		killSession(in.PID)
		up := time.Since(in.StartedAt)
		detail := reason
		if forced {
			detail = strings.TrimPrefix(detail+"; killed after stop timeout", "; ")
		}
		runCleanup(in.Cleanup, in)
		switch {
		case !in.waitChipsFree(in.StopTimeout + 30*time.Second):
			detail = strings.TrimPrefix(detail+"; chips still busy after the stop, not reset", "; ")
		case forced && in.ResetOnKill:
			detail += "; " + in.reset("killed")
		}
		record("stop", in, &up, detail)
	} else {
		in.noteExit()
	}
	if err := os.Remove(in.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// runCleanup runs cleanup commands, ignoring failures (the thing to clean may be gone);
// output goes to the instance log when there is one.
func runCleanup(cmds [][]string, in *Instance) {
	for _, c := range cmds {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, err := exec.CommandContext(ctx, c[0], c[1:]...).CombinedOutput()
		cancel()
		if in != nil {
			if f, ferr := os.OpenFile(in.Log, os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
				fmt.Fprintf(f, "# substrate cleanup: %s: err=%v %s\n", ShellJoin(c), err, strings.TrimSpace(string(out)))
				f.Close()
			}
		}
	}
}

// waitChipsFree waits until the instance's chips are unlocked and unopened: helpers
// outside its session (a docker client and container, an engine's own lock holder)
// may outlive the process substrate stopped. It reports whether they came free.
func (in *Instance) waitChipsFree(max time.Duration) bool {
	if in.Accelerator != spec.AccelTenstorrent || len(in.Devices) == 0 {
		return true
	}
	deadline := time.Now().Add(max)
	for {
		busy := false
		for _, d := range in.Devices {
			if TT.Held(d) || len(tenstorrent.Users(d)) > 0 {
				busy = true
			}
		}
		if !busy {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// reset resets the instance's Tenstorrent chips and describes the outcome for the event log.
func (in *Instance) reset(why string) string {
	if in.Accelerator != spec.AccelTenstorrent || len(in.Devices) == 0 {
		return ""
	}
	if err := TT.Reset(in.Devices); err != nil {
		return fmt.Sprintf("chip reset after %s failed: %v\n", why, err)
	}
	return fmt.Sprintf("chips %s reset after %s\n", tenstorrent.Join(in.Devices), why)
}

// killSession SIGKILLs every process left in the engine's session (sid == leader pid).
func killSession(sid int) {
	victims := append(SessionPIDs(sid), sid)
	syscall.Kill(-sid, syscall.SIGKILL)
	for _, p := range victims {
		syscall.Kill(p, syscall.SIGKILL)
	}
	// Wait (up to 10s) until they are gone and the driver no longer lists them, so their
	// GPU memory is free before anyone re-plans (a restart would otherwise evict needlessly).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(SessionPIDs(sid)) > 0 {
		time.Sleep(100 * time.Millisecond)
	}
	for time.Now().Before(deadline) && holdsGPU(victims) {
		time.Sleep(250 * time.Millisecond)
	}
}

func holdsGPU(pids []int) bool {
	gs, err := gpu.List()
	if err != nil || len(gs) == 0 {
		return false
	}
	u, err := gpu.ProcessUsage(gs)
	if err != nil {
		return false
	}
	return len(u.Of(pids)) > 0
}

// SessionPIDs lists live processes whose session id is sid (an engine and all its workers).
func SessionPIDs(sid int) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	want := strconv.Itoa(sid)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		// Fields after ')': state ppid pgrp session ...
		if f := stat(pid); len(f) > 3 && f[3] == want && f[0] != "Z" {
			out = append(out, pid)
		}
	}
	return out
}

// RSSMiB sums resident memory of pids.
func RSSMiB(pids []int) int64 {
	var kb int64
	for _, p := range pids {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", p))
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
				n, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
				kb += n
			}
		}
	}
	return kb / 1024
}

// Forget removes the record of an exited instance (recording its exit first).
func (in *Instance) Forget() error {
	return lock.With(lockFor(in.ID), func() error {
		if in.Alive() {
			return errs.New(errs.Conflict, "%s is running", in.ID)
		}
		in.noteExit()
		err := os.Remove(in.path())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

// Tail returns the last n lines of a file.
func Tail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	// Read at most the last 256 KiB: logs of long-running engines grow large.
	const window = 256 << 10
	if st, err := f.Stat(); err == nil && st.Size() > window {
		f.Seek(st.Size()-window, 0)
	}
	data, _ := io.ReadAll(f)
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte("\n")))
}

// ShellJoin quotes argv for display.
func ShellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\"'$\\;&|<>(){}*?") {
			q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			q[i] = a
		}
	}
	return strings.Join(q, " ")
}
