package mockforge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// The endpoints follow the HTTP API of Prometheus:
// https://prometheus.io/docs/prometheus/latest/querying/api/
// (instant query, range query and /api/v1/alerts).
//
// The mock does not parse PromQL. A health value is a number that a test sets.
// The value holds from the time of the call until the next call, so a test makes
// a service healthy or broken after a deploy. A value can have its own query
// text. A query with no value of its own gets the default value. With no value
// at all, the mock answers 1, which is healthy.

const maxMockPoints = 11000

type promPoint struct {
	At     time.Time
	Value  string
	NoData bool
}

type promState struct {
	health map[string][]promPoint // the key is a query, or "" for the default
	alerts []json.RawMessage
}

// sample is a number or a string in JSON, for example 0, 1.5 or "NaN".
type sample string

// UnmarshalJSON reads a number or a string.
func (s *sample) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = sample(str)
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("a value is a number or a string: %w", err)
	}
	*s = sample(strconv.FormatFloat(f, 'g', -1, 64))
	return nil
}

// valueAt returns the value of a query at a time.
func (p *promState) valueAt(query string, t time.Time) (string, bool) {
	hist, ok := p.health[query]
	if !ok {
		hist = p.health[""]
	}
	if len(hist) == 0 {
		return "1", true
	}
	cur := hist[0]
	for _, pt := range hist {
		if !pt.At.After(t) {
			cur = pt
		}
	}
	return cur.Value, !cur.NoData
}

func (m *Mock) controlProm(w http.ResponseWriter, req *http.Request, what string) {
	if req.Method != http.MethodPost {
		notFound(w)
		return
	}
	switch what {
	case "health":
		var in struct {
			Query  string  `json:"query"`
			Value  sample  `json:"value"`
			NoData bool    `json:"no_data"`
			At     *string `json:"at"`
		}
		if !readBody(w, req, &in) {
			return
		}
		at := m.now()
		if in.At != nil {
			t, err := time.Parse(time.RFC3339Nano, *in.At)
			if err != nil {
				apiError(w, http.StatusBadRequest, "at must be an RFC 3339 time")
				return
			}
			at = t
		}
		if in.Value == "" {
			in.Value = "1"
		}
		hist := append([]promPoint{}, m.prom.health[in.Query]...)
		hist = append(hist, promPoint{At: at, Value: string(in.Value), NoData: in.NoData})
		sort.SliceStable(hist, func(i, j int) bool { return hist[i].At.Before(hist[j].At) })
		m.prom.health[in.Query] = hist
		writeJSON(w, http.StatusOK, map[string]any{"query": in.Query, "value": string(in.Value), "at": stamp(at)})
	case "alerts":
		var in struct {
			Alerts []json.RawMessage `json:"alerts"`
		}
		if readBody(w, req, &in) {
			m.prom.alerts = in.Alerts
			writeJSON(w, http.StatusOK, map[string]int{"alerts": len(in.Alerts)})
		}
	default:
		notFound(w)
	}
}

func (m *Mock) promView() map[string]any {
	out := map[string]any{}
	for q, hist := range m.prom.health {
		list := make([]map[string]any, 0, len(hist))
		for _, pt := range hist {
			list = append(list, map[string]any{"at": stamp(pt.At), "value": pt.Value, "no_data": pt.NoData})
		}
		out[q] = list
	}
	return map[string]any{"health": out, "alerts": len(m.prom.alerts)}
}

func promError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"status": "error", "errorType": "bad_data", "error": msg})
}

func (m *Mock) prometheus(w http.ResponseWriter, req *http.Request, what string) {
	if what == "alerts" && req.Method == http.MethodGet {
		alerts := m.prom.alerts
		if alerts == nil {
			alerts = []json.RawMessage{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "data": map[string]any{"alerts": alerts}})
		return
	}
	if req.Method != http.MethodPost && req.Method != http.MethodGet {
		notFound(w)
		return
	}
	if err := req.ParseForm(); err != nil {
		promError(w, http.StatusBadRequest, "the form is not valid")
		return
	}
	query := req.Form.Get("query")
	if query == "" {
		promError(w, http.StatusBadRequest, "invalid parameter \"query\": no expression")
		return
	}
	switch what {
	case "query":
		m.instant(w, req, query)
	case "query_range":
		m.rangeQuery(w, req, query)
	default:
		notFound(w)
	}
}

func parseProm(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(int64(f), int64((f-float64(int64(f)))*1e9)), true
}

func unixString(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func (m *Mock) instant(w http.ResponseWriter, req *http.Request, query string) {
	at := m.now()
	if s := req.Form.Get("time"); s != "" {
		t, ok := parseProm(s)
		if !ok {
			promError(w, http.StatusBadRequest, "invalid parameter \"time\"")
			return
		}
		at = t
	}
	result := []map[string]any{}
	if v, ok := m.prom.valueAt(query, at); ok {
		result = append(result, map[string]any{"metric": map[string]string{}, "value": []any{unixString(at), v}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
}

func (m *Mock) rangeQuery(w http.ResponseWriter, req *http.Request, query string) {
	start, ok1 := parseProm(req.Form.Get("start"))
	end, ok2 := parseProm(req.Form.Get("end"))
	step, ok3 := parseStepProm(req.Form.Get("step"))
	switch {
	case !ok1 || !ok2:
		promError(w, http.StatusBadRequest, "invalid parameter \"start\" or \"end\"")
		return
	case !ok3 || step <= 0:
		promError(w, http.StatusBadRequest, "invalid parameter \"step\"")
		return
	case end.Before(start):
		promError(w, http.StatusBadRequest, "end timestamp must not be before start time")
		return
	case float64(end.Sub(start))/float64(step) > maxMockPoints:
		promError(w, http.StatusBadRequest, "exceeded maximum resolution of 11,000 points per timeseries")
		return
	}
	var values [][]any
	for t := start; !t.After(end); t = t.Add(step) {
		if v, ok := m.prom.valueAt(query, t); ok {
			values = append(values, []any{unixString(t), v})
		}
	}
	result := []map[string]any{}
	if len(values) > 0 {
		result = append(result, map[string]any{"metric": map[string]string{}, "values": values})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
}

func parseStepProm(s string) (time.Duration, bool) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second)), true
	}
	d, err := time.ParseDuration(s)
	return d, err == nil
}
