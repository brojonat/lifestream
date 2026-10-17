package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/brojonat/lifestream/internal/store"
)

// chartDays is how much history the metric charts show.
const chartDays = 90

const week = 7 * 24 * time.Hour

// ---- metrics page --------------------------------------------------------

// metricCard is one metric collection: a tile and a chart.
type metricCard struct {
	ID         string
	Name       string
	Unit       string
	Closed     bool
	Count      int
	Latest     *store.MetricValue
	ThisWeek   *float64 // mean of the last 7 days
	LastWeek   *float64 // mean of the 7 days before that
	Change     *float64 // ThisWeek - LastWeek
	Points     string   // JSON [[unix ms, value], ...] for the chart
	Average    string
	ChartLabel string
	Rows       []tableRow // the chart as a table, newest first
}

type tableRow struct {
	At      time.Time
	Value   float64
	Average float64
}

type metricsData struct {
	pageData
	Owner string
	Cards []metricCard
}

func (s *Server) metricsView(r *http.Request) (any, error) {
	ctx := r.Context()
	u, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := s.Events.Collections(ctx, u.ID, "")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	data := metricsData{pageData: s.base("Metrics", "/stream", "metrics"), Owner: u.Name}
	for _, c := range cs {
		if c.Kind != store.KindMetric {
			continue
		}
		vals, err := s.Events.MetricValues(ctx, c.ID, now.AddDate(0, 0, -chartDays))
		if err != nil {
			return nil, err
		}
		data.Cards = append(data.Cards, buildCard(c, vals, now))
	}
	return data, nil
}

func buildCard(c store.Collection, vals []store.MetricValue, now time.Time) metricCard {
	card := metricCard{ID: c.ID, Name: c.Name, Unit: c.Unit, Closed: c.Status != "open", Count: len(vals)}
	if len(vals) > 0 {
		card.Latest = &vals[len(vals)-1]
		if card.Unit == "" {
			card.Unit = card.Latest.Unit
		}
	}
	card.ThisWeek = mean(vals, now.Add(-week), now)
	card.LastWeek = mean(vals, now.Add(-2*week), now.Add(-week))
	if card.ThisWeek != nil && card.LastWeek != nil {
		d := *card.ThisWeek - *card.LastWeek
		card.Change = &d
	}
	points, avg := series(vals)
	card.Points, card.Average = jsonString(points), jsonString(avg)
	for i := len(vals) - 1; i >= 0; i-- {
		card.Rows = append(card.Rows, tableRow{At: vals[i].ObservedAt.Local(), Value: vals[i].Value, Average: avg[i][1]})
	}
	card.ChartLabel = fmt.Sprintf("%s over the last %d days, with a trailing 7-day average", c.Name, chartDays)
	return card
}

// mean of the values observed in [from, to), or nil if there are none.
func mean(vals []store.MetricValue, from, to time.Time) *float64 {
	sum, n := 0.0, 0
	for _, v := range vals {
		if !v.ObservedAt.Before(from) && v.ObservedAt.Before(to) {
			sum += v.Value
			n++
		}
	}
	if n == 0 {
		return nil
	}
	m := sum / float64(n)
	return &m
}

// series returns the chart's points ([unix ms, value]) and, at each point,
// the mean of the readings in the 7 days ending there. vals is sorted by
// time.
func series(vals []store.MetricValue) (points, avg [][2]float64) {
	points, avg = [][2]float64{}, [][2]float64{}
	start, sum := 0, 0.0
	for i, v := range vals {
		sum += v.Value
		for !vals[start].ObservedAt.After(v.ObservedAt.Add(-week)) { // window is (t-7d, t]
			sum -= vals[start].Value
			start++
		}
		t := float64(v.ObservedAt.UnixMilli())
		points = append(points, [2]float64{t, v.Value})
		avg = append(avg, [2]float64{t, round(sum/float64(i-start+1), 2)})
	}
	return points, avg
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// num formats a reading: up to two decimals, no trailing zeros.
func num(v float64) string { return strconv.FormatFloat(round(v, 2), 'f', -1, 64) }

func signed(v float64) string {
	if v > 0 {
		return "+" + num(v)
	}
	return num(v)
}

// ---- inbox page ------------------------------------------------------------

// inboxRow is a raw event and what it became, flattened for the template.
type inboxRow struct {
	ID       string
	At       time.Time
	Source   string
	Text     string
	Status   string // waiting, filed, flagged, ignored
	Labels   []store.Membership
	Values   []string
	Flags    []string
	Redacted bool
}

type inboxData struct {
	pageData
	Rows  []inboxRow
	Open  []store.Collection // targets for "add to…"
	Flags []flagRow          // questions waiting for the owner
}

const inboxSize = 50

func (s *Server) inboxView(r *http.Request) (any, error) {
	ctx := r.Context()
	u, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.Events.Inbox(ctx, u.ID, inboxSize)
	if err != nil {
		return nil, err
	}
	open, err := s.Events.Collections(ctx, u.ID, "open")
	if err != nil {
		return nil, err
	}
	flags, err := s.flags(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	data := inboxData{pageData: s.base("Inbox", "/inbox/stream", "inbox"), Open: open, Flags: flags}
	for _, it := range items {
		data.Rows = append(data.Rows, buildRow(it))
	}
	return data, nil
}

func buildRow(it store.InboxItem) inboxRow {
	e := it.Event
	row := inboxRow{ID: e.ID, At: e.OccurredAt, Source: e.Source, Text: eventText(e), Labels: it.Members}
	for _, v := range it.Values {
		row.Values = append(row.Values, strings.TrimSpace(num(v.Value)+" "+v.Unit))
	}
	for _, d := range it.Derived {
		switch d.Type {
		case "review.flagged":
			var p struct {
				Question string `json:"question"`
				Reason   string `json:"reason"`
			}
			json.Unmarshal(d.Payload, &p)
			row.Flags = append(row.Flags, firstNonEmpty(p.Question, p.Reason, "needs review"))
		case "redaction.created":
			row.Redacted = true
		}
	}
	switch {
	case len(row.Flags) > 0:
		row.Status = "flagged"
	case len(it.Derived) > 0 || len(it.Members) > 0:
		row.Status = "filed"
	case !it.Delivered:
		row.Status = "waiting"
	default:
		row.Status = "ignored"
	}
	return row
}

// eventText is the human part of a raw event: an SMS body, a note's text,
// or else the payload itself.
func eventText(e store.Event) string {
	var p map[string]any
	if json.Unmarshal(e.Payload, &p) == nil {
		for _, k := range []string{"Body", "text", "body", "title"} {
			if s, ok := p[k].(string); ok {
				return s
			}
		}
	}
	return string(e.Payload)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
