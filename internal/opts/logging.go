package opts

import (
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Zap is the logging configuration both controllers use.
//
// StacktraceLevel is raised to Panic because controller-runtime's default puts a stack on every
// Error, burying an already-wrapped reconcile error on each retry. Errors stay at Error level
// (ADR 0046).
func Zap() zap.Options {
	return zap.Options{Development: false, StacktraceLevel: zapcore.PanicLevel}
}
