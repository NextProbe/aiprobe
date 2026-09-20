package trace

import (
	"bytes"
	"encoding/json"
)

// MarshalJSON emits the value, or null when the quantity is genuinely unknown.
// Inventing precise numbers is forbidden, so "unknown" must survive all the
// way into the JSON report.
func (o OptInt) MarshalJSON() ([]byte, error) {
	if !o.Known {
		return []byte("null"), nil
	}
	return json.Marshal(o.Value)
}

// UnmarshalJSON accepts a number or null.
func (o *OptInt) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		*o = OptInt{}
		return nil
	}
	var v int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*o = OptInt{Value: v, Known: true}
	return nil
}

// MarshalJSON emits the value, or null when unknown.
func (o OptFloat) MarshalJSON() ([]byte, error) {
	if !o.Known {
		return []byte("null"), nil
	}
	return json.Marshal(round4(o.Value))
}

// UnmarshalJSON accepts a number or null.
func (o *OptFloat) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		*o = OptFloat{}
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*o = OptFloat{Value: v, Known: true}
	return nil
}

func round4(v float64) float64 {
	const scale = 10000
	if v >= 0 {
		return float64(int64(v*scale+0.5)) / scale
	}
	return float64(int64(v*scale-0.5)) / scale
}
