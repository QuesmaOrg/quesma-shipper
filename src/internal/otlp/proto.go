package otlp

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"

	"google.golang.org/protobuf/encoding/protowire"
)

// Decoded by field number against opentelemetry-proto v1, so no generated code is linked in.
var errProto = errors.New("otlp: malformed protobuf")

// field is one decoded field: num and exactly one of varint/fixed (in n) or bytes (in b).
type field struct {
	num protowire.Number
	typ protowire.Type
	n   uint64
	b   []byte
}

func fields(b []byte) ([]field, error) {
	var out []field
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, errProto
		}
		b = b[n:]
		f := field{num: num, typ: typ}
		switch typ {
		case protowire.VarintType:
			f.n, n = protowire.ConsumeVarint(b)
		case protowire.Fixed64Type:
			f.n, n = protowire.ConsumeFixed64(b)
		case protowire.Fixed32Type:
			var v uint32
			v, n = protowire.ConsumeFixed32(b)
			f.n = uint64(v)
		case protowire.BytesType:
			f.b, n = protowire.ConsumeBytes(b)
		default:
			return nil, errProto
		}
		if n < 0 {
			return nil, errProto
		}
		b = b[n:]
		out = append(out, f)
	}
	return out, nil
}

func anyValue(b []byte) (any, error) {
	fs, err := fields(b)
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		switch f.num {
		case 1:
			return string(f.b), nil
		case 2:
			return f.n != 0, nil
		case 3:
			return int64(f.n), nil
		case 4:
			return math.Float64frombits(f.n), nil
		case 5:
			vs, err := fields(f.b)
			if err != nil {
				return nil, err
			}
			out := make([]any, 0, len(vs))
			for _, v := range vs {
				x, err := anyValue(v.b)
				if err != nil {
					return nil, err
				}
				out = append(out, x)
			}
			return out, nil
		case 6:
			return keyValues(f.b, 1)
		case 7:
			return hex.EncodeToString(f.b), nil
		}
	}
	return nil, nil
}

// keyValues collects the repeated KeyValue at field num of message b.
func keyValues(b []byte, num protowire.Number) (map[string]any, error) {
	fs, err := fields(b)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, f := range fs {
		if f.num != num {
			continue
		}
		kv, err := fields(f.b)
		if err != nil {
			return nil, err
		}
		var key string
		var value any
		for _, x := range kv {
			switch x.num {
			case 1:
				key = string(x.b)
			case 2:
				if value, err = anyValue(x.b); err != nil {
					return nil, err
				}
			}
		}
		out[key] = value
	}
	return out, nil
}

func nanos(n uint64) string { return strconv.FormatUint(n, 10) }

func scopeName(b []byte) string {
	fs, _ := fields(b)
	for _, f := range fs {
		if f.num == 1 {
			return string(f.b)
		}
	}
	return ""
}

// walk visits resource (1) and scope (2) groups shared by both requests, calling leaf for each
// record at field 2 of a scope group with a resource map of its own.
func walk(body []byte, leaf func(resource func() (map[string]any, error), scope string, b []byte) error) error {
	req, err := fields(body)
	if err != nil {
		return err
	}
	for _, rg := range req {
		if rg.num != 1 {
			continue
		}
		group, err := fields(rg.b)
		if err != nil {
			return err
		}
		var resourceBytes []byte
		for _, f := range group {
			if f.num == 1 {
				resourceBytes = f.b
			}
		}
		resource := func() (map[string]any, error) { return keyValues(resourceBytes, 1) }
		for _, sg := range group {
			if sg.num != 2 {
				continue
			}
			scoped, err := fields(sg.b)
			if err != nil {
				return err
			}
			var scope string
			for _, f := range scoped {
				if f.num == 1 {
					scope = scopeName(f.b)
				}
			}
			for _, f := range scoped {
				if f.num == 2 {
					if err := leaf(resource, scope, f.b); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func decodeProto(signal Signal, body []byte) ([]record, error) {
	var out []record
	var err error
	switch signal {
	case Logs:
		err = walk(body, func(resource func() (map[string]any, error), scope string, b []byte) error {
			fs, err := fields(b)
			if err != nil {
				return err
			}
			f := map[string]any{"signal": "log", "time_unix_nano": "0", "observed_time_unix_nano": "0"}
			for _, x := range fs {
				switch x.num {
				case 1:
					f["time_unix_nano"] = nanos(x.n)
				case 11:
					f["observed_time_unix_nano"] = nanos(x.n)
				case 3:
					setIf(f, "severity", string(x.b))
				case 5:
					body, err := anyValue(x.b)
					if err != nil {
						return err
					}
					if body != nil {
						f["body"] = body
					}
				case 9:
					setIf(f, "trace_id", hex.EncodeToString(x.b))
				case 10:
					setIf(f, "span_id", hex.EncodeToString(x.b))
				case 12:
					setIf(f, "event", string(x.b))
				}
			}
			attrs, err := keyValues(b, 6)
			if err != nil {
				return err
			}
			res, err := resource()
			if err != nil {
				return err
			}
			out = append(out, record{res, scope, f, attrs})
			return nil
		})
	case Metrics:
		err = walk(body, func(resource func() (map[string]any, error), scope string, b []byte) error {
			fs, err := fields(b)
			if err != nil {
				return err
			}
			var name, unit string
			for _, x := range fs {
				switch x.num {
				case 1:
					name = string(x.b)
				case 3:
					unit = string(x.b)
				}
			}
			for _, x := range fs {
				kind := map[protowire.Number]string{5: "gauge", 7: "sum", 9: "histogram"}[x.num]
				if kind == "" {
					continue
				}
				data, err := fields(x.b)
				if err != nil {
					return err
				}
				var temp int64
				var monotonic bool
				for _, d := range data {
					switch d.num {
					case 2:
						temp = int64(d.n)
					case 3:
						monotonic = d.n != 0
					}
				}
				for _, d := range data {
					if d.num != 1 {
						continue
					}
					f := map[string]any{"signal": "metric", "metric": name, "unit": unit, "kind": kind,
						"start_time_unix_nano": "0", "time_unix_nano": "0"}
					setIf(f, "temporality", temporality(temp))
					if kind == "sum" {
						f["monotonic"] = monotonic
					}
					attrNum := protowire.Number(7)
					if kind == "histogram" {
						attrNum = 9
					}
					pt, err := fields(d.b)
					if err != nil {
						return err
					}
					for _, p := range pt {
						switch {
						case p.num == 2:
							f["start_time_unix_nano"] = nanos(p.n)
						case p.num == 3:
							f["time_unix_nano"] = nanos(p.n)
						case kind == "histogram" && p.num == 4:
							f["count"] = int64(p.n)
						case kind == "histogram" && p.num == 5:
							f["sum"] = math.Float64frombits(p.n)
						case kind != "histogram" && p.num == 4:
							f["value"] = math.Float64frombits(p.n)
						case kind != "histogram" && p.num == 6:
							f["value"] = int64(p.n)
						}
					}
					attrs, err := keyValues(d.b, attrNum)
					if err != nil {
						return err
					}
					res, err := resource()
					if err != nil {
						return err
					}
					out = append(out, record{res, scope, f, attrs})
				}
			}
			return nil
		})
	default:
		return nil, fmt.Errorf("otlp: unsupported signal %q", signal)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}
