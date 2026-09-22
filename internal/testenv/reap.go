// Package testenv runs and cleans up envtest for the integration suites of both controller
// packages.
//
// controller-runtime's testEnv.Stop() signals its children, which Windows does not support, so
// every run would leave an etcd and a kube-apiserver behind; ReapChildren closes that gap.
package testenv

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// ReapChildren force-kills any envtest process still running as a child of this test binary.
//
// Scoped to OUR children: `go test ./...` runs both integration suites concurrently, and a blanket
// kill would take down the other's apiserver. A no-op except on Windows; errors are ignored, as
// the process is exiting anyway.
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
