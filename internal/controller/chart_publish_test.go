package controller

import (
	"strings"
	"testing"
)

// TestTheControllersNeverGetThePublicHost pins the internal/public split: `--default-registry` is
// the in-cluster Service (cluster DNS may not resolve the public name), and the public name travels
// only as `--public-registry-host`, for status.artifact.ref.
func TestTheControllersNeverGetThePublicHost(t *testing.T) {
	const public = "oci-composer.internal:30500"

	out := render(t,
		"--set", "registry.publish.mode=nodePort",
		"--set", "registry.host="+public,
		"--set", "registry.service.type=NodePort",
		"--set", "registry.service.nodePort=30500",
	)

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- --default-registry=") && strings.Contains(line, public) {
			t.Errorf("--default-registry names the PUBLIC host; the controllers cannot resolve it: %s", line)
		}
		if strings.HasPrefix(line, "- --insecure-registry=") && strings.Contains(line, public) {
			t.Errorf("--insecure-registry names the public host, which may well terminate TLS: %s", line)
		}
	}

	if !strings.Contains(out, "--public-registry-host="+public) {
		t.Error("the public host must reach the controllers as --public-registry-host, or status.artifact.ref is unpullable")
	}
	if !strings.Contains(out, "--default-registry=test-release-kube-oci-composer-registry.oci-composer.svc.cluster.local:5000") {
		t.Errorf("--default-registry must be the in-cluster Service:\n%s", grepFlags(out))
	}
	// Without TLS the Service speaks plain HTTP, so it is insecure in every mode, public host or not.
	if !strings.Contains(out, "--insecure-registry=test-release-kube-oci-composer-registry.oci-composer.svc.cluster.local:5000") {
		t.Errorf("the in-cluster Service must be marked insecure; without it the controllers try TLS against plain HTTP:\n%s", grepFlags(out))
	}
}

// TestNoPublicHostFlagWhenThereIsNoPublicHost: an unset public host renders no flag rather than an
// empty one.
func TestNoPublicHostFlagWhenThereIsNoPublicHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"internalOnly", []string{"--set", "registry.publish.mode=internalOnly"}},
		{"external", []string{
			"--set", "registry.publish.mode=external",
			"--set", "registry.enabled=false",
			"--set", "defaultRegistry.host=ghcr.io/example",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out := render(t, tc.args...); strings.Contains(out, "--public-registry-host") {
				t.Errorf("no public host is configured, so no flag should render:\n%s", grepFlags(out))
			}
		})
	}
}

// TestChartRefusesAnUndecidedPublishMode: no publish mode is right on every cluster, so the chart
// refuses at install time rather than producing images that fail with ErrImagePull later.
func TestChartRefusesAnUndecidedPublishMode(t *testing.T) {
	out := renderRawExpectingFailure(t)

	if !strings.Contains(out, "registry.publish.mode") {
		t.Fatalf("the refusal must name the value to set:\n%s", out)
	}
	// The refusal must name every option.
	for _, mode := range []string{"ingress", "nodePort", "external", "internalOnly"} {
		if !strings.Contains(out, mode) {
			t.Errorf("the refusal must offer %q as an option; got:\n%s", mode, out)
		}
	}
}

// TestEachPublishModeAssertsWhatMakesItTrue: a mode must refuse to render without the values it
// depends on, or it produces images nothing can pull.
func TestEachPublishModeAssertsWhatMakesItTrue(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"an unknown mode", []string{"--set", "registry.publish.mode=wat"}, "not one of"},
		{"ingress without a host", []string{"--set", "registry.publish.mode=ingress"}, "registry.host"},
		{"nodePort without a host", []string{"--set", "registry.publish.mode=nodePort"}, "registry.host"},
		{
			"nodePort without a fixed port",
			[]string{"--set", "registry.publish.mode=nodePort", "--set", "registry.host=oci-composer.internal:30500"},
			"nodePort",
		},
		{
			"external while the bundled registry is still installed",
			[]string{"--set", "registry.publish.mode=external"},
			"registry.enabled=false",
		},
		{
			"external without a registry to publish to",
			[]string{"--set", "registry.publish.mode=external", "--set", "registry.enabled=false"},
			"defaultRegistry.host",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderRawExpectingFailure(t, tc.args...)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("the render failed, but not for this reason — wanted %q in:\n%s", tc.want, out)
			}
		})
	}
}

// TestTheIngressServesTheWholeRegistryAPI: a path-scoped Ingress passes the version check and then
// fails blob uploads.
func TestTheIngressServesTheWholeRegistryAPI(t *testing.T) {
	out := render(t,
		"--set", "registry.publish.mode=ingress",
		// A port here is legal in a registry reference and illegal in an Ingress rule host.
		"--set", "registry.host=oci.example.com:8443",
	)
	if !strings.Contains(out, "kind: Ingress") {
		t.Fatal("publish.mode=ingress must render an Ingress")
	}
	if strings.Contains(out, "host: \"oci.example.com:8443\"") || strings.Contains(out, "host: oci.example.com:8443") {
		t.Error("the Ingress rule host must have the port stripped, or it matches nothing")
	}
	if !strings.Contains(out, "path: /") {
		t.Error("the Ingress must route the whole registry API, not a /v2/ prefix")
	}
}

// grepFlags trims a render down to the controller arguments, so a failure message is readable.
func grepFlags(out string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "- --") {
			b.WriteString(t)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
