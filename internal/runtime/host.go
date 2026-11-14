package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/spec"
)

// HostedModels lists the models multi-model host in has loaded now.
func HostedModels(ctx context.Context, in *Instance, h *spec.Host) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, err := hostCall(ctx, in, http.MethodGet, h.Models, nil)
	if err != nil {
		return nil, err
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, errs.New(errs.EngineFailed, "%s %s: %v", in.ID, h.Models, err)
	}
	names := make([]string, len(list))
	for i, m := range list {
		names[i] = m.Name
	}
	return names, nil
}

// LoadHosted asks host in to load the model at path and serve it as name, with a
// context of ctxLen tokens (0: the host's default). It returns once the model serves.
func LoadHosted(ctx context.Context, in *Instance, h *spec.Host, name, path string, ctxLen int) error {
	ctx, cancel := context.WithTimeout(ctx, h.LoadTimeout.Or(30*time.Minute))
	defer cancel()
	_, err := hostCall(ctx, in, http.MethodPost, h.Load, map[string]any{"name": name, "path": path, "ctx": ctxLen})
	return err
}

// UnloadHosted asks host in to drop model name; unloading a model it does not hold is not an error.
func UnloadHosted(ctx context.Context, in *Instance, h *spec.Host, name string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, err := hostCall(ctx, in, http.MethodPost, h.Unload, map[string]any{"name": name})
	var he *hostError
	if errors.As(err, &he) && he.status == http.StatusNotFound {
		return nil
	}
	return err
}

type hostError struct {
	status int
	msg    string
}

func (e *hostError) Error() string { return e.msg }

func hostCall(ctx context.Context, in *Instance, method, path string, payload any) ([]byte, error) {
	var rd io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, in.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if in.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+in.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errs.New(errs.Timeout, "%s %s %s: no answer in time", in.ID, method, path)
		}
		return nil, errs.New(errs.EngineFailed, "%s %s %s: %v", in.ID, method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, errs.New(errs.EngineFailed, "%s %s %s: %v", in.ID, method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return nil, errs.Wrap(errs.EngineFailed, &hostError{resp.StatusCode, fmt.Sprintf("%s %s %s: %d %s", in.ID, method, path, resp.StatusCode, msg)})
	}
	return body, nil
}
