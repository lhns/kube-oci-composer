package opts

import (
	"testing"

	"go.uber.org/zap/zapcore"
)

// TestErrorsCarryNoStacktrace: Errors carry no stack (ADR 0046), panics still do.
func TestErrorsCarryNoStacktrace(t *testing.T) {
	got := Zap()
	if got.StacktraceLevel == nil {
		t.Fatal("StacktraceLevel is unset, so the production default puts a stack on every Error")
	}
	if got.StacktraceLevel.Enabled(zapcore.ErrorLevel) {
		t.Error("Error still carries a stacktrace")
	}
	if !got.StacktraceLevel.Enabled(zapcore.PanicLevel) {
		t.Error("a panic would carry no stacktrace, which is the one case worth having one")
	}
}
