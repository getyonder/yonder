package nearby

import (
	"io"
	"time"
)

// SetRandom replaces where a client's keys and salts come from, so a test
// can know them in advance.
func (c *Client) SetRandom(r io.Reader) { c.random = r }

// SetClock replaces the clock of the limit on pairing requests.
func (c *Client) SetClock(now func() time.Time) { c.now = now }
