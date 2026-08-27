package opts

import (
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Zap is the logging configuration both controllers use.
//
// Shared rather than written twice: the setting below is the kind that gets fixed in one binary and
// silently left in the other, and the only symptom is noisier logs.
//
// StacktraceLevel is the whole reason this exists. controller-runtime's production default attaches
// a Go stack at Error, and a reconcile error does not benefit: it is already wrapped with the
// context that matters, so the trace buries the message under twelve frames of controller-runtime
// on every retry of every failing object. The LEVEL stays Error -- a failing build is worth one --
// because the severity was never what was wrong. Panics still trace.
func Zap() zap.Options {
	return zap.Options{Development: false, StacktraceLevel: zapcore.PanicLevel}
}
