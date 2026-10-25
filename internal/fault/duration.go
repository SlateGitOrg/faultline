package fault

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is a time.Duration that encodes to and decodes from Go duration
// strings ("150ms", "2s") in JSON. Plain JSON numbers are accepted on input
// and interpreted as nanoseconds, matching time.Duration's own encoding.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String renders the duration in Go duration syntax.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON encodes the duration as a string such as "150ms".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a duration string or a number of nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, perr := time.ParseDuration(s)
		if perr != nil {
			return fmt.Errorf("invalid duration %q: %w", s, perr)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"150ms\" or integer nanoseconds: %s", string(b))
	}
	*d = Duration(n)
	return nil
}
