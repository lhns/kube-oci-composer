{{- /*
registry.publish.mode has no default, and the chart refuses to install without it.

That is a deliberate cost, so here is the argument for paying it.

status.artifact.ref is a single string that TWO resolvers have to understand. The controllers reach
the registry through cluster DNS to push and refresh; the kubelet reaches it with the NODE's
resolver to pull, and the node's resolver has never heard of anything.svc.cluster.local. Before the
internal/public split there was no value that worked: leave the host unset and every pull failed
with ErrImagePull, set it to a node-resolvable name and every publish failed with "no such host".

The split fixed the mechanism. It cannot fix the fact that SOMEBODY has to say how nodes reach the
registry, because that depends on a cluster this chart cannot see -- whether there is an ingress
controller, whether there is DNS for it, whether the operator is willing to put a file on every
node. There is no answer that works everywhere, so guessing produces the failure this file exists
to prevent: an install that reports success and produces images nothing can pull, discovered days
later by someone who did not install it.

So the chart asks. The failure moves from a silent runtime one to a message at install time naming
the four choices, which is the whole trade.
*/}}

{{- define "kube-oci-composer.checkPublishMode" -}}
{{- $mode := .Values.registry.publish.mode | default "" -}}
{{- $valid := list "ingress" "nodePort" "external" "internalOnly" -}}

{{- if not $mode -}}
{{- fail `registry.publish.mode is not set, and there is no safe default.

It says how WORKLOADS reach the registry. The controllers do not need it -- they always use the
in-cluster Service -- but a kubelet resolves image references with the NODE's resolver, which
cannot see cluster DNS, so without this the chart would publish images no Pod could pull.

  registry.publish.mode=ingress       an Ingress is rendered; set registry.host to its hostname
  registry.publish.mode=nodePort      set registry.host to a name your nodes resolve, and
                                      registry.service.nodePort; each node needs a containerd
                                      certs.d drop-in (see docs/registry.md)
  registry.publish.mode=external      you run the registry: registry.enabled=false plus
                                      defaultRegistry.host and defaultRegistry.existingPushSecret
  registry.publish.mode=internalOnly  nothing outside the cluster pulls these images, on purpose` -}}
{{- end -}}

{{- if not (has $mode $valid) -}}
{{- fail (printf "registry.publish.mode is %q, which is not one of: %s" $mode (join ", " $valid)) -}}
{{- end -}}

{{- /*
Each mode asserts the values that make it true. A mode that renders without them is a mode that
lied -- the operator answered the question and still got images nothing can pull.
*/}}
{{- if and (eq $mode "ingress") (not .Values.registry.host) -}}
{{- fail "registry.publish.mode=ingress needs registry.host set to the hostname the Ingress serves; it is what workloads will pull from." -}}
{{- end -}}

{{- if eq $mode "nodePort" -}}
{{- if not .Values.registry.host -}}
{{- fail "registry.publish.mode=nodePort needs registry.host set to a name your NODES resolve to this registry. It is not a cluster DNS name -- containerd resolves image references with the node's resolver. See docs/registry.md." -}}
{{- end -}}
{{- if not .Values.registry.service.nodePort -}}
{{- fail "registry.publish.mode=nodePort needs registry.service.nodePort, so the containerd drop-in on each node has a fixed port to point at." -}}
{{- end -}}
{{- end -}}

{{- if eq $mode "external" -}}
{{- if .Values.registry.enabled -}}
{{- fail "registry.publish.mode=external means you run the registry, so set registry.enabled=false. Leaving the bundled one installed would run a registry nothing publishes to." -}}
{{- end -}}
{{- if not .Values.defaultRegistry.host -}}
{{- fail "registry.publish.mode=external needs defaultRegistry.host set to your registry, and usually defaultRegistry.existingPushSecret with it." -}}
{{- end -}}
{{- end -}}

{{- end -}}
