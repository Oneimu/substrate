// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resources

import (
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"k8s.io/apimachinery/pkg/api/resource"
)

// RoomCheck asks one fixed question of many Workers in a row: does what a
// Worker has left admit one more Actor of this size? Placement asks it of the
// whole fleet on every call, so a RoomCheck parses the Actor's size once and
// then reads each Worker's wire form in place, with no Quantities map per
// Worker. It also memoizes every quantity string it has parsed: a fleet
// reports a handful of distinct capacities and its allocation totals are sums
// of a handful of template sizes, so past the first few Workers a check
// parses nothing at all.
//
// A RoomCheck belongs to one goroutine. Its memo and scratch space are not
// locked.
type RoomCheck struct {
	// names and need are the Actor's size, one dimension per index. A linear
	// scan over names beats a map for the two or three dimensions a size has.
	names []string
	need  []resource.Quantity
	// free is scratch for one Worker's remaining capacity, index-aligned with
	// names, reused across Workers so a check allocates nothing.
	free []resource.Quantity
	// parsed memoizes ParseQuantity by wire string, failures included, so a
	// string seen on one Worker is never parsed again for another.
	parsed map[string]parsedQuantity
}

type parsedQuantity struct {
	quantity resource.Quantity
	ok       bool
}

// NewRoomCheck prepares to place an Actor asking for want. It errors, as
// ParseQuantities does, on a quantity it cannot parse.
func NewRoomCheck(want *ateapipb.Resources) (*RoomCheck, error) {
	quantities, err := ParseQuantities(want)
	if err != nil {
		return nil, err
	}
	c := &RoomCheck{
		names:  make([]string, 0, len(quantities)),
		need:   make([]resource.Quantity, 0, len(quantities)),
		free:   make([]resource.Quantity, len(quantities)),
		parsed: make(map[string]parsedQuantity),
	}
	for name, need := range quantities {
		c.names = append(c.names, name)
		c.need = append(c.need, int64Form(need))
	}
	return c, nil
}

// Admits reports whether capacity less allocated covers the Actor's size in
// every dimension the size names, with the semantics of ParseQuantities, Sub,
// and Covers: a repeated name sums, a dimension the Worker does not report is
// none of it, and an overcommitted dimension covers nothing. An Actor asking
// for nothing fits anywhere, whatever the Worker reports.
//
// A capacity or allocation entry that will not parse, wanted dimension or
// not, means no room: the Worker's true occupancy is unreadable.
func (c *RoomCheck) Admits(capacity, allocated *ateapipb.Resources) bool {
	if len(c.names) == 0 {
		return true
	}
	clear(c.free)
	for _, limit := range capacity.GetLimits() {
		q, ok := c.parse(limit.GetQuantity())
		if !ok {
			return false
		}
		if i := c.index(limit.GetName()); i >= 0 {
			c.free[i].Add(q)
		}
	}
	for _, limit := range allocated.GetLimits() {
		q, ok := c.parse(limit.GetQuantity())
		if !ok {
			return false
		}
		if i := c.index(limit.GetName()); i >= 0 {
			c.free[i].Sub(q)
		}
	}
	for i := range c.names {
		if c.free[i].Cmp(c.need[i]) < 0 {
			return false
		}
	}
	return true
}

func (c *RoomCheck) index(name string) int {
	for i, n := range c.names {
		if n == name {
			return i
		}
	}
	return -1
}

func (c *RoomCheck) parse(s string) (resource.Quantity, bool) {
	p, seen := c.parsed[s]
	if !seen {
		q, err := resource.ParseQuantity(s)
		p = parsedQuantity{quantity: int64Form(q), ok: err == nil}
		c.parsed[s] = p
	}
	return p.quantity, p.ok
}

// int64Form returns q backed by a plain int64 amount when its value fits one at
// whole, milli, micro, or nano scale. That is every quantity short of a
// billion-fold spread between its magnitude and its precision, since
// ParseQuantity rounds to nano scale; anything else is returned as is, still
// correct, just slower to compare.
//
// ParseQuantity leaves a quantity with a fractional or long mantissa, "1.5Gi"
// say, on its inf.Dec path, where every Add, Sub, and Cmp allocates big.Int
// state, and where the pointer inside q would be shared between the memo and
// every Worker checked against it. The int64 form is a value: arithmetic on a
// copy allocates nothing and touches nothing shared.
func int64Form(q resource.Quantity) resource.Quantity {
	for _, scale := range []resource.Scale{0, resource.Milli, resource.Micro, resource.Nano} {
		// ScaledValue rounds up and silently overflows; comparing the result
		// back against q accepts it only when it is exact. Cmp against an
		// inf.Dec rewrites its receiver into one, hence the throwaway copy.
		candidate := *resource.NewScaledQuantity(q.ScaledValue(scale), scale)
		if compared := candidate; compared.Cmp(q) == 0 {
			return candidate
		}
	}
	return q
}
