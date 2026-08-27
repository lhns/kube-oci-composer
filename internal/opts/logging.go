package opts

import (
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Zap is the logging configuration both controllers use. Shared because the kind of setting below
// gets fixed in one binary and silently left in the other.
//
// StacktraceLevel is the whole reason this exists: controller-runtime attaches a Go stack at Error,
// which buries an already-wrapped reconcile error under twelve frames on every retry. The LEVEL
// stays Error -- the severity was never what was wrong. Panics still trace. ADR 0046.
func Zap() zap.Options {
	return zap.Options{Development: false, StacktraceLevel: zapcore.PanicLevel}
}
