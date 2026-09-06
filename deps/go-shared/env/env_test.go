package env

import "testing"

func TestNormalizePort(t *testing.T) {
	cases := map[string]string{
		"5432":                 "5432",
		"tcp://10.0.0.1:5432":  "5432",
		"unix:///run/x.sock:1": "1",
		":9999":                "9999",
		"bad:":                 "bad:", // empty port after colon → unchanged
	}
	for in, want := range cases {
		if got := NormalizePort(in); got != want {
			t.Errorf("NormalizePort(%q) = %q, want %q", in, got, want)
		}
	}
}
