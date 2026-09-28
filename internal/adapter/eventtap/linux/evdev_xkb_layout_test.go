//go:build linux && cgo

package linux

import "testing"

func TestReferenceKeyboardLayout_Set(t *testing.T) {
	t.Parallel()

	const (
		english = "English (US)"
		dvorak  = "English (Dvorak)"
	)

	var layout referenceKeyboardLayout
	if !layout.set(english) {
		t.Fatal("startup must accept a pending override before proxy warm-up")
	}
	// Retain requests made before a keymap arrives so the reader can apply them.
	if got := *layout.name.Load(); got != english {
		t.Fatalf("pending layout = %q", got)
	}

	names := []string{english, dvorak}
	layout.available.Store(&names)

	for _, test := range []struct {
		id   string
		want string
		ok   bool
	}{
		{"english (us)", english, true},
		{dvorak, dvorak, true},
		{"missing", "missing", false},
		{"first", "first", false},
		{"current", "current", false},
		{"", "", true},
	} {
		if got := layout.set(test.id); got != test.ok {
			t.Errorf("set(%q) = %v, want %v", test.id, got, test.ok)
		}

		if got := *layout.name.Load(); got != test.want {
			t.Errorf("set(%q): name = %q, want %q", test.id, got, test.want)
		}
	}
}
