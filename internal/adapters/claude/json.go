package claude

import (
	"bytes"
	"encoding/json"
)

// jsonUnmarshal decodes s into v, using json.Number so large integers in
// free-form tool arguments survive a round trip without float artifacts.
func jsonUnmarshal(s string, v any) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	return dec.Decode(v)
}

// jsonMarshal re-serializes a decoded value. Map keys are sorted by
// encoding/json, which keeps tool-argument fingerprints stable across runs.
func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
