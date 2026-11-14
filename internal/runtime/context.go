package runtime

import (
	"context"
	"net/http"
	"time"
)

// Report asks the running engine for what it says about itself at path (its manifest names the path for the
// model's context window and for image input); nil when it does not answer.
func Report(ctx context.Context, in *Instance, path string) []byte {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	body, err := hostCall(ctx, in, http.MethodGet, path, nil)
	if err != nil {
		return nil
	}
	return body
}
