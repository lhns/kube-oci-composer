# Verifying what this operator signs

**These files are the half that makes the other half not theatre.**

ADR 0008 says it plainly and ADR 0040 repeats it without softening: a signature changes nothing an
attacker experiences until something refuses to run what it cannot verify. This project ships no
admission policy — it produces signatures, and enforcing them is a separate component and a
separate decision. What follows is a working starting point for the two most common enforcers.

## First, the public key

```console
kubectl -n oci-composer get secret cosign-signing-key -o jsonpath='{.data.cosign\.pub}' | base64 -d
```

That is the half you give an admission controller. The private half stays in the controller's
namespace and is never projected into a build pod — code from a git repository never runs in the
same container as the key.

## What a signature here does and does not mean

**It means: this operator produced this image.** It does not mean anyone approved it. Any tenant who
can create an `ImageComposition` in any namespace gets the operator's signature on whatever they
composed, because the operator is what published it. The signature attests provenance, not review,
and it is not a substitute for RBAC on the CRD.

So a policy that requires this signature is saying "this image came out of our pipeline rather than
from somewhere else". That is a genuinely useful thing to require and a much narrower one than it
first sounds.

## Kyverno

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: require-composer-signature
spec:
  validationFailureAction: Enforce
  webhookTimeoutSeconds: 30
  rules:
    - name: verify-composed-images
      match:
        any:
          - resources:
              kinds: [Pod]
      verifyImages:
        # Scope it to the registry this operator publishes to. Requiring a signature on EVERY image
        # would reject upstream images nobody here signs -- including this operator's own.
        - imageReferences:
            - "oci-composer.internal:30500/*"
          attestors:
            - entries:
                - keys:
                    publicKeys: |-
                      -----BEGIN PUBLIC KEY-----
                      REPLACE WITH cosign.pub
                      -----END PUBLIC KEY-----
                    # This project signs with a key, not keyless, so there is no transparency log
                    # to consult -- ADR 0008 rejected keyless because it would publish private
                    # image names and digests to a public ledger.
                    rekor:
                      ignoreTlog: true
```

## Sigstore policy-controller

```yaml
apiVersion: policy.sigstore.dev/v1beta1
kind: ClusterImagePolicy
metadata:
  name: require-composer-signature
spec:
  images:
    - glob: "oci-composer.internal:30500/**"
  authorities:
    - key:
        data: |-
          -----BEGIN PUBLIC KEY-----
          REPLACE WITH cosign.pub
          -----END PUBLIC KEY-----
      ctlog:
        url: ""   # key-based, no transparency log
```

## Checking by hand first

Before enforcing anything, confirm the signature is there and verifies. Enforcing a policy against
images that are not signed the way you think they are is a cluster-wide outage.

```console
cosign verify --key cosign.pub --insecure-ignore-tlog \
  oci-composer.internal:30500/team-a/app@sha256:...

# and the attestations, if enabled
cosign download attestation \
  oci-composer.internal:30500/team-a/app@sha256:... | jq -r '.payload' | base64 -d | jq
```

`--insecure-ignore-tlog` is not a workaround here: it is what key-based signing means. There is no
transparency log entry because nothing was published to one, deliberately.

## Roll it out in the order that cannot break the cluster

1. Turn on signing in the chart and let it run. Nothing enforces yet.
2. Verify a few images by hand, as above.
3. Install the policy with `validationFailureAction: Audit` (Kyverno) or `warn` mode, and read the
   reports for a while — long enough for anything scheduled rarely to have been scheduled.
4. Only then enforce.

Step 3 is the one people skip. The images that break a signature policy are the ones nothing has
rescheduled recently, so a policy that looks clean on Tuesday can take out a `CronJob` in a month.
