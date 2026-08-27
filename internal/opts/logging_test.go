package opts

import (
	"testing"

	"go.uber.org/zap/zapcore"
)

// TestErrorsCarryNoStacktrace — the noise this removed.
//
// controller-runtime's production default attaches a Go stack at Error, so every retry of every
// failing build printed twelve frames of controller-runtime around the one line worth reading. The
// LEVEL is deliberately unchanged: a failing build is worth an error, and lowering it to Info to
// dodge the formatting would have misreported the severity instead of fixing it.
func TestErrorsCarryNoStacktrace(t *testing.T) {
	got := Zap()
	if got.StacktraceLevel == nil {
		t.Fatal("StacktraceLevel is unset, so the production default puts a stack on every Error")
	}
	if got.StacktraceLevel.Enabled(zapcore.ErrorLevel) {
		t.Error("Error still carries a stacktrace")
	}
	// Panics keep theirs -- that is the case a stack is actually for.
	if !got.StacktraceLevel.Enabled(zapcore.PanicLevel) {
		t.Error("a panic would carry no stacktrace, which is the one case worth having one")
	}
}
