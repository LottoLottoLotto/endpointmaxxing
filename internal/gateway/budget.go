package gateway

import (
	"errors"
	"fmt"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
)

// errBudget refuses a request whose team spent a token or cost budget (429, error_kind budget_exceeded).
var errBudget = errors.New("team budget exhausted")

// teamUse is a team's running totals for the current day and month.
type teamUse struct {
	day, month  string
	dayTokens   int64
	monthTokens int64
	monthCost   float64
	checked     time.Time
	alerted     map[string]string // limit -> period it was alerted in
}

func thisMonth() (string, time.Time) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return start.Format("2006-01"), start
}

// teamUsage returns team's totals, re-reading the database when the period turned or the cache is stale.
// The caller holds a.mu.
func (a *authState) teamUsage(team string) *teamUse {
	if a.teams == nil {
		a.teams = map[string]*teamUse{}
	}
	day, dayStart := today()
	month, monthStart := thisMonth()
	u := a.teams[team]
	if u != nil && u.day == day && u.month == month && time.Since(u.checked) <= tokenRecheck {
		return u
	}
	alerted := map[string]string{}
	if u != nil {
		alerted = u.alerted
	}
	u = &teamUse{day: day, month: month, checked: time.Now(), alerted: alerted}
	if a.dbFunc != nil {
		if db, err := a.dbFunc(); err == nil {
			if d, err := db.TeamSince(team, dayStart); err == nil {
				u.dayTokens = d.Tokens
			}
			if m, err := db.TeamSince(team, monthStart); err == nil {
				u.monthTokens, u.monthCost = m.Tokens, m.Cost
			}
		}
	}
	a.teams[team] = u
	return u
}

// budgetLimit is one team limit with its current use.
type budgetLimit struct {
	name      string // tokens_per_day, tokens_per_month, budget_per_month
	used, max float64
	period    string
}

func teamLimits(t *config.Team, u *teamUse) []budgetLimit {
	var out []budgetLimit
	if t.TokensPerDay > 0 {
		out = append(out, budgetLimit{"tokens_per_day", float64(u.dayTokens), float64(t.TokensPerDay), u.day})
	}
	if t.TokensPerMonth > 0 {
		out = append(out, budgetLimit{"tokens_per_month", float64(u.monthTokens), float64(t.TokensPerMonth), u.month})
	}
	if t.BudgetPerMonth > 0 {
		out = append(out, budgetLimit{"budget_per_month", u.monthCost, t.BudgetPerMonth, u.month})
	}
	return out
}

// admitTeam refuses a request when the team has used up any of its limits, returning the wait until the
// latest-resetting exhausted limit renews.
func (a *authState) admitTeam(t *config.Team, currency string) (time.Duration, error) {
	if t == nil {
		return 0, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var wait time.Duration
	var refusal error
	for _, l := range teamLimits(t, a.teamUsage(t.Name)) {
		if l.used < l.max {
			continue
		}
		reset := untilNextMonth()
		if l.name == "tokens_per_day" {
			reset = untilNextDay()
		}
		if reset > wait {
			wait = reset
		}
		if refusal == nil {
			used := fmt.Sprintf("%.0f of %.0f tokens", l.used, l.max)
			if l.name == "budget_per_month" {
				used = fmt.Sprintf("%.2f of %.2f %s", l.used, l.max, currency)
			}
			refusal = errs.Wrap(errs.Capacity, fmt.Errorf("team %s: %s %s used (%s): %w", t.Name, used, periodWord(l.name), l.name, errBudget))
		}
	}
	return wait, refusal
}

func periodWord(limit string) string {
	if limit == "tokens_per_day" {
		return "today"
	}
	return "this month"
}

// budgetAlert is a team crossing its alert fraction of a limit.
type budgetAlert struct {
	team string
	l    budgetLimit
}

// chargeTeam adds a finished request to its team's totals and returns alerts newly crossed.
// The caller holds a.mu.
func (a *authState) chargeTeam(r *store.Request, t *config.Team) []budgetAlert {
	if t == nil {
		return nil
	}
	u := a.teamUsage(t.Name)
	var n int64
	if r.PromptTokens != nil {
		n += *r.PromptTokens
	}
	if r.CompletionTokens != nil {
		n += *r.CompletionTokens
	}
	u.dayTokens += n
	u.monthTokens += n
	if r.Cost != nil {
		u.monthCost += *r.Cost
	}
	var out []budgetAlert
	for _, l := range teamLimits(t, u) {
		if l.used >= t.AlertFraction()*l.max && u.alerted[l.name] != l.period {
			u.alerted[l.name] = l.period
			out = append(out, budgetAlert{t.Name, l})
		}
	}
	return out
}

// raiseBudgetAlerts logs each alert and records it as a budget_alert event.
func (g *Gateway) raiseBudgetAlerts(alerts []budgetAlert, currency string) {
	for _, al := range alerts {
		used := fmt.Sprintf("%.0f of %.0f tokens", al.l.used, al.l.max)
		if al.l.name == "budget_per_month" {
			used = fmt.Sprintf("%.2f of %.2f %s", al.l.used, al.l.max, currency)
		}
		msg := fmt.Sprintf("team %s: %.0f%% of %s (%s, %s)", al.team, 100*al.l.used/al.l.max, al.l.name, used, al.l.period)
		g.Log.Printf("budget alert: %s", msg)
		g.recordEvent("budget_alert", msg)
	}
}

// TeamStatus is a team's limits and current use, for reports and metrics.
type TeamStatus struct {
	Name   string      `json:"name"`
	Keys   int         `json:"keys"`
	Limits []TeamLimit `json:"limits"`
	Config config.Team `json:"config"`
}

// TeamLimit is one limit's use in its current period.
type TeamLimit struct {
	Limit  string  `json:"limit"`
	Used   float64 `json:"used"`
	Max    float64 `json:"max"`
	Ratio  float64 `json:"ratio"`
	Period string  `json:"period"`
}

// TeamStatuses reports every configured team's limits against the usage database (nil db: no use).
func TeamStatuses(cfg *config.Config, db *store.DB, keysPerTeam map[string]int) ([]TeamStatus, error) {
	day, dayStart := today()
	month, monthStart := thisMonth()
	out := make([]TeamStatus, 0, len(cfg.Teams))
	for i := range cfg.Teams {
		t := &cfg.Teams[i]
		u := &teamUse{day: day, month: month}
		if db != nil {
			d, err := db.TeamSince(t.Name, dayStart)
			if err != nil {
				return nil, err
			}
			m, err := db.TeamSince(t.Name, monthStart)
			if err != nil {
				return nil, err
			}
			u.dayTokens, u.monthTokens, u.monthCost = d.Tokens, m.Tokens, m.Cost
		}
		st := TeamStatus{Name: t.Name, Keys: keysPerTeam[t.Name], Config: *t, Limits: []TeamLimit{}}
		for _, l := range teamLimits(t, u) {
			st.Limits = append(st.Limits, TeamLimit{Limit: l.name, Used: l.used, Max: l.max, Ratio: l.used / l.max, Period: l.period})
		}
		out = append(out, st)
	}
	return out, nil
}
