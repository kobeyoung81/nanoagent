package main

// A fresh catalog starts with the tools this binary ships, and only those: the
// list is explicit so a tool added later does not silently become available to
// every agent on the next deploy.

import "testing"

func TestBuiltinDefaultEnabled(t *testing.T) {
	for _, name := range []string{"read", "write", "bash", "grep", "parallel_execution", "question"} {
		if !builtinDefaultEnabled(name) {
			t.Errorf("%s starts off; want on", name)
		}
	}
	for _, name := range []string{"", "not_a_tool", "mcp__something"} {
		if builtinDefaultEnabled(name) {
			t.Errorf("%q starts on; want off", name)
		}
	}
}
