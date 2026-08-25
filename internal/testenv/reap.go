// Package testenv cleans up after envtest.
//
// It exists because controller-runtime's testEnv.Stop() cannot stop its children on Windows: it
// signals them, and signalling is "not supported by windows", so every run leaves an etcd and a
// kube-apiserver behind. Thirty-two of them accumulated during one afternoon's work before anyone
// looked at Task Manager.
//
// Imported only by the integration suites. It is a normal package rather than a _test.go helper
// because there are two of them, in different packages.
package testenv

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// ReapChildren force-kills any envtest process still running as a child of this test binary.
//
// Scoped to OUR children, not to every etcd on the machine. `go test ./...` runs each package's
// binary concurrently, so both integration suites can have an apiserver up at once and a blanket
// `taskkill /IM etcd.exe` from one would kill the other's mid-run. envtest starts both processes
// with os/exec, so they are direct children and the PID filter is exact.
//
// A no-op everywhere but Windows, where the signal that Stop() sends does not exist. Errors are
// discarded throughout: this runs when the test process is already on its way out, and there is
// nothing useful to do about a failure to kill something that may already be gone.
func ReapChildren() {
	if runtime.GOOS != "windows" {
		return
	}
	script := fmt.Sprintf(
		`Get-CimInstance Win32_Process -Filter "ParentProcessId=%d" | `+
			`Where-Object { $_.Name -in 'etcd.exe','kube-apiserver.exe' } | `+
			`ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`,
		os.Getpid())
	_ = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Run()
}
