package otlp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type jAny struct {
	StringValue *string         `json:"stringValue"`
	BoolValue   *bool           `json:"boolValue"`
	IntValue    json.RawMessage `json:"intValue"`
	DoubleValue json.RawMessage `json:"doubleValue"`
	ArrayValue  *struct {
		Values []jAny `json:"values"`
	} `json:"arrayValue"`
	KvlistValue *struct {
		Values []jKV `json:"values"`
	} `json:"kvlistValue"`
	BytesValue *string `json:"bytesValue"`
}

type jKV struct {
	Key   string `json:"key"`
	Value jAny   `json:"value"`
}

type jResource struct {
	Attributes []jKV `json:"attributes"`
}

type jScope struct {
	Name string `json:"name"`
}

type jLogs struct {
	ResourceLogs []struct {
		Resource  jResource `json:"resource"`
		ScopeLogs []struct {
			Scope      jScope `json:"scope"`
			LogRecords []struct {
				TimeUnixNano         json.RawMessage `json:"timeUnixNano"`
				ObservedTimeUnixNano json.RawMessage `json:"observedTimeUnixNano"`
				SeverityText         string          `json:"severityText"`
				EventName            string          `json:"eventName"`
				Body                 *jAny           `json:"body"`
				Attributes           []jKV           `json:"attributes"`
				TraceID              string          `json:"traceId"`
				SpanID               string          `json:"spanId"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

type jNumberPoint struct {
	Attributes        []jKV           `json:"attributes"`
	StartTimeUnixNano json.RawMessage `json:"startTimeUnixNano"`
	TimeUnixNano      json.RawMessage `json:"timeUnixNano"`
	AsDouble          json.RawMessage `json:"asDouble"`
	AsInt             json.RawMessage `json:"asInt"`
	Count             json.RawMessage `json:"count"`
	Sum               json.RawMessage `json:"sum"`
}

type jPoints struct {
	DataPoints             []jNumberPoint  `json:"dataPoints"`
	AggregationTemporality json.RawMessage `json:"aggregationTemporality"`
	IsMonotonic            bool            `json:"isMonotonic"`
}

type jMetrics struct {
	ResourceMetrics []struct {
		Resource     jResource `json:"resource"`
		ScopeMetrics []struct {
			Scope   jScope `json:"scope"`
			Metrics []struct {
				Name      string   `json:"name"`
				Unit      string   `json:"unit"`
				Gauge     *jPoints `json:"gauge"`
				Sum       *jPoints `json:"sum"`
				Histogram *jPoints `json:"histogram"`
			} `json:"metrics"`
		} `json:"scopeMetrics"`
	} `json:"resourceMetrics"`
}

// OTLP JSON writes 64-bit integers as strings; some exporters write numbers. Both become digits.
func digits(raw json.RawMessage) string {
	return strings.Trim(string(bytes.TrimSpace(raw)), `"`)
}

func number(raw json.RawMessage) any {
	s := digits(raw)
	if s == "" {
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func (a jAny) value() any {
	switch {
	case a.StringValue != nil:
		return *a.StringValue
	case a.BoolValue != nil:
		return *a.BoolValue
	case a.IntValue != nil:
		return number(a.IntValue)
	case a.DoubleValue != nil:
		return number(a.DoubleValue)
	case a.ArrayValue != nil:
		out := make([]any, 0, len(a.ArrayValue.Values))
		for _, v := range a.ArrayValue.Values {
			out = append(out, v.value())
		}
		return out
	case a.KvlistValue != nil:
		return attrsJSON(a.KvlistValue.Values)
	case a.BytesValue != nil:
		return *a.BytesValue
	}
	return nil
}

func attrsJSON(kvs []jKV) map[string]any {
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		out[kv.Key] = kv.Value.value()
	}
	return out
}

func decodeJSON(signal Signal, body []byte) ([]record, error) {
	var out []record
	switch signal {
	case Logs:
		var req jLogs
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("otlp: %w", err)
		}
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					f := map[string]any{"signal": "log", "time_unix_nano": digits(lr.TimeUnixNano),
						"observed_time_unix_nano": digits(lr.ObservedTimeUnixNano)}
					if lr.Body != nil && lr.Body.value() != nil {
						f["body"] = lr.Body.value()
					}
					setIf(f, "event", lr.EventName)
					setIf(f, "severity", lr.SeverityText)
					setIf(f, "trace_id", lr.TraceID)
					setIf(f, "span_id", lr.SpanID)
					// A fresh resource map per record: Decode lifts claims out of it.
					out = append(out, record{attrsJSON(rl.Resource.Attributes), sl.Scope.Name, f, attrsJSON(lr.Attributes)})
				}
			}
		}
	case Metrics:
		var req jMetrics
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("otlp: %w", err)
		}
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					for kind, pts := range map[string]*jPoints{"gauge": m.Gauge, "sum": m.Sum, "histogram": m.Histogram} {
						if pts == nil {
							continue
						}
						temp, _ := number(pts.AggregationTemporality).(int64)
						for _, dp := range pts.DataPoints {
							f := map[string]any{"signal": "metric", "metric": m.Name, "unit": m.Unit, "kind": kind,
								"start_time_unix_nano": digits(dp.StartTimeUnixNano), "time_unix_nano": digits(dp.TimeUnixNano)}
							setIf(f, "temporality", temporality(temp))
							if kind == "sum" {
								f["monotonic"] = pts.IsMonotonic
							}
							if kind == "histogram" {
								f["count"], f["sum"] = number(dp.Count), number(dp.Sum)
							} else if dp.AsInt != nil {
								f["value"] = number(dp.AsInt)
							} else {
								f["value"] = number(dp.AsDouble)
							}
							out = append(out, record{attrsJSON(rm.Resource.Attributes), sm.Scope.Name, f, attrsJSON(dp.Attributes)})
						}
					}
				}
			}
		}
	default:
		return nil, fmt.Errorf("otlp: unsupported signal %q", signal)
	}
	return out, nil
}

func setIf(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}
