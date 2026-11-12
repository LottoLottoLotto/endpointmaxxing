package cli

import (
	"github.com/LottoLottoLotto/substrate/internal/gateway"
	"github.com/LottoLottoLotto/substrate/internal/settings"
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

// applied reports a settings change: its warnings on stderr and its admin-trail record.
func (e *env) applied(r settings.Result) {
	for _, w := range r.Warnings {
		e.warn("%s", w)
	}
	e.recordAdmin(r.Detail)
}
