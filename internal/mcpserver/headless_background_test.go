package mcpserver

import "testing"

func TestRunsInBackgroundRule(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	b := func(v bool) *bool { return &v }
	cases := []struct {
		name string
		in   headlessInput
		want bool
	}{
		{"omitted timeout runs in the foreground", headlessInput{}, false},
		{"timeout of 120 stays in the foreground", headlessInput{Timeout: f(120)}, false},
		{"timeout over 120 goes to the background", headlessInput{Timeout: f(121)}, true},
		{"an explicit 600 goes to the background", headlessInput{Timeout: f(600)}, true},
		{"background true without a timeout", headlessInput{Background: b(true)}, true},
		{"background false beats a long timeout", headlessInput{Timeout: f(900), Background: b(false)}, false},
	}
	for _, c := range cases {
		if got := runsInBackground(c.in); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
