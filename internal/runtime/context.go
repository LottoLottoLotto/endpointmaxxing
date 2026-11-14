package runtime

import (
	"context"
	"net/http"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/spec"
)

// ReportedContext asks the running engine for the context window of the model it serves, the way its
// manifest says to; 0 when the manifest does not say, or the engine does not answer with one.
func ReportedContext(ctx context.Context, in *Instance, c *spec.Context) int {
	if c == nil || c.Path == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	body, err := hostCall(ctx, in, http.MethodGet, c.Path, nil)
	if err != nil {
		return 0
	}
	return c.Read(body)
}
