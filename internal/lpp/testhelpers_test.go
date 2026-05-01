package lpp

import "encoding/json"

// mustUnmarshal is a tiny helper used across the lpp test suite.
func mustUnmarshal(raw string, into any) error {
	return json.Unmarshal([]byte(raw), into)
}
