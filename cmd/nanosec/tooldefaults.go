package main

// Which built-in tools a fresh capability catalog starts with enabled.
//
// The catalog is a two-plane thing: a tool's availability default lives in the
// definition plane (the builtin_tools row), and each agent type may decide for
// itself. A binary that announces no default hands every tool the zero value,
// which is "off" — that is why a fresh installation used to come up with a
// build agent that could do almost nothing: the type belt names only the few
// tools it must have, and everything else inherited the catalog's off.
//
// The list below is the explicit counterpart. It is a list and not "everything
// the registry happens to hold" on purpose: a tool added to the registry later
// must be opted in here, so a new capability does not silently become available
// to every agent on the next deploy.
var defaultOnBuiltinTools = map[string]bool{
	"append_note":        true,
	"bash":               true,
	"cron":               true,
	"download_media":     true,
	"edit":               true,
	"glob":               true,
	"grep":               true,
	"list":               true,
	"load_skill":         true,
	"manage_skill":       true,
	"parallel_execution": true,
	"plan_execute":       true,
	"plan_write":         true,
	"question":           true,
	"read":               true,
	"readonly_bash":      true,
	"readonly_task":      true,
	"task":               true,
	"todowrite":          true,
	"upload_media":       true,
	"webfetch":           true,
	"write":              true,
}

// builtinDefaultEnabled reports whether a built-in tool starts enabled for the
// agent types that do not decide about it themselves. Unknown names are off.
func builtinDefaultEnabled(name string) bool {
	return defaultOnBuiltinTools[name]
}
