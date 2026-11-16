package api

import "testing"

// The terminal pump types everything it does not recognise into the
// remote shell, so what it refuses to recognise has to be decided by the
// frame's "type" and nothing else. The two regressions this pins: a
// keystroke that begins with '{', and a pasted JSON document that merely
// happens to carry cols/rows.
func TestMachineControlFrameNeedsAType(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
		kind string
	}{
		{"resize", `{"type":"resize","cols":132,"rows":43}`, true, "resize"},
		{"resize without dims", `{"type":"resize"}`, false, ""},
		{"resize with zero rows", `{"type":"resize","cols":80,"rows":0}`, false, ""},
		{"zmodem arm", `{"type":"zmodem","paths":["/tmp/x"]}`, true, "zmodem"},
		{"zmodem cancel", `{"type":"zmodem","action":"cancel"}`, true, "zmodem"},
		// A shell one-liner, typed one character at a time and flushed as
		// one write: the leading brace is the shell's, not JSON's.
		{"shell braces", `{ ls; }`, false, ""},
		{"pasted package.json", `{"cols": 80, "rows": 24}`, false, ""},
		{"unknown type", `{"type":"ping"}`, false, ""},
		{"no type", `{"paths":["/tmp/x"]}`, false, ""},
		{"plain keystroke", "q", false, ""},
		{"lone brace", `{`, false, ""},
		{"empty", ``, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame, ok := machineControlFrame([]byte(tt.msg))
			if ok != tt.want {
				t.Fatalf("machineControlFrame(%q) ok = %v, want %v", tt.msg, ok, tt.want)
			}
			if frame.Type != tt.kind {
				t.Errorf("machineControlFrame(%q) type = %q, want %q", tt.msg, frame.Type, tt.kind)
			}
		})
	}
}
