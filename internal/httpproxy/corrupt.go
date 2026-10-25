package httpproxy

import (
	"io"
	"math"

	"github.com/SlateGitOrg/faultline/internal/fault"
)

// corruptBody wraps a response body and mangles it as it streams.
//
// Bit-flip mode flips one random bit in each selected byte, selecting bytes
// independently with probability Rate. Instead of rolling a die per byte it
// draws the gap to the next corrupted byte from a geometric distribution,
// so the cost is O(flips) rather than O(bytes).
//
// Truncate mode ends the stream after AfterBytes bytes. If the upstream sent
// a Content-Length the client observes an unexpected EOF, which is exactly
// what a connection dropped mid-body looks like.
type corruptBody struct {
	src   io.ReadCloser
	spec  *fault.Active
	rng   *fault.RNG
	pos   int64
	next  int64 // absolute offset of the next byte to flip
	limit int64 // truncation offset, -1 for none
}

func newCorruptBody(src io.ReadCloser, a *fault.Active, rng *fault.RNG) io.ReadCloser {
	c := &corruptBody{src: src, spec: a, rng: rng, limit: -1}
	if a.Mode == fault.CorruptTruncate {
		c.limit = a.AfterBytes
	} else {
		c.next = c.gap()
	}
	return c
}

// gap draws the number of untouched bytes before the next flip.
func (c *corruptBody) gap() int64 {
	p := c.spec.FlipRate()
	if p >= 1 {
		return 0
	}
	u := c.rng.Float64()
	if u == 0 {
		u = math.SmallestNonzeroFloat64
	}
	g := math.Floor(math.Log(u) / math.Log1p(-p))
	if g > math.MaxInt32 {
		return math.MaxInt32
	}
	return int64(g)
}

func (c *corruptBody) Read(b []byte) (int, error) {
	if c.limit >= 0 {
		remaining := c.limit - c.pos
		if remaining <= 0 {
			return 0, io.EOF
		}
		if int64(len(b)) > remaining {
			b = b[:remaining]
		}
	}
	n, err := c.src.Read(b)
	if c.limit < 0 {
		for c.next < c.pos+int64(n) {
			i := c.next - c.pos
			b[i] ^= 1 << uint(c.rng.IntN(8)) //nolint:gosec // IntN(8) is within [0,8)
			c.next += 1 + c.gap()
		}
	}
	c.pos += int64(n)
	if c.limit >= 0 && c.pos >= c.limit && err == nil {
		err = io.EOF
	}
	return n, err
}

func (c *corruptBody) Close() error { return c.src.Close() }
