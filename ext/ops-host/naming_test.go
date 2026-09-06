package main

import (
	"strings"
	"testing"
)

func TestValidComponent(t *testing.T) {
	cases := map[string]bool{
		"main": true, "acme": true, "feature-x": true, "my.repo": true,
		"v1.2": true, "a.b.c": true, "Feature_X": true, "a": true,
		"": false, "-lead": false, "has:colon": false, "dot..dot": false,
		"trailing.": false, "name.lock": false, "has space": false,
		"has/slash": true, "feature/a": true, "中文": false,
		"/lead": false, "trail/": false, "a//b": false,
		strings.Repeat("a", 128): true,
		strings.Repeat("a", 129): false,
		"0start":                 true,
	}
	for in, want := range cases {
		if got := validComponent(in); got != want {
			t.Errorf("validComponent(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseSessionName(t *testing.T) {
	if o, r, b, ok := parseSessionName("acme:webapp:feature-x"); !ok || o != "acme" || r != "webapp" || b != "feature-x" {
		t.Errorf("parse ok triple failed: %v %v %v %v", o, r, b, ok)
	}
	for _, bad := range []string{"plain", "a:b", "a:b:c:d", "a::c", "a:b:c d"} {
		if _, _, _, ok := parseSessionName(bad); ok {
			t.Errorf("parseSessionName(%q) should fail", bad)
		}
	}
}

func TestSessionKey(t *testing.T) {
	a, b := sessionKey("acme:repo-a:main"), sessionKey("acme:repo-b:main")
	if a == b {
		t.Fatal("session keys collide for similar sessions")
	}
	if a != sessionKey("acme:repo-a:main") {
		t.Fatal("sessionKey not deterministic")
	}
	if strings.ContainsAny(a, ":") {
		t.Fatal("sessionKey must be k8s-safe (no colon)")
	}
}
