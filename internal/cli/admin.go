package cli

import (
	"fmt"
	"time"

	"github.com/lotto/substrate/internal/gateway"
)

// recordAdmin records an administrative change made by this command (events table, audit log, webhooks).
// A failure to record never fails the change itself; it is reported on stderr.
func (e *env) recordAdmin(detail string) {
	a, err := e.app()
	if err != nil {
		e.warn("recording the change: %v", err)
		return
	}
	gateway.RecordAdmin(a, gateway.Actor(), detail)
}

func expiryText(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func cachedText(p *float64) string {
	if p == nil {
		return "= prompt"
	}
	return fmt.Sprintf("%g", *p)
}
