package main

import "testing"

// C3 predicate: the no-budget-fuse warning fires only when the cumulative token fuse is unset AND the run is
// long-session-prone — a cross-turn -session resume (cumulative spend) or iterations raised above the default 20.
// Default single turns are left alone (soft fuses already bound them).
func TestShouldWarnBudgetFuse(t *testing.T) {
	cases := []struct {
		name          string
		maxTotalSet   bool
		session       string
		maxIterations int
		want          bool
	}{
		{"fuse set, default single", true, "", 20, false},
		{"fuse set, resume+high iter", true, "id", 300, false},
		{"unset, default single (iter 20)", false, "", 20, false},
		{"unset, iter 0 (means default 20)", false, "", 0, false},
		{"unset, iter raised to 21", false, "", 21, true},
		{"unset, iter 300", false, "", 300, true},
		{"unset, resume at default iter", false, "id", 0, true},
		{"unset, resume iter 20", false, "id", 20, true},
	}
	for _, c := range cases {
		if got := shouldWarnBudgetFuse(c.maxTotalSet, c.session, c.maxIterations); got != c.want {
			t.Errorf("%s: shouldWarnBudgetFuse(%v, %q, %d) = %v, want %v", c.name, c.maxTotalSet, c.session, c.maxIterations, got, c.want)
		}
	}
}
