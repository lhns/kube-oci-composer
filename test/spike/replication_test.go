//go:build spike

// Package spike falsifies the premise of a replicated registry: N zot processes serving one shared
// store.
//
// Run it against test/spike/compose.yaml — three zot instances over ONE Docker volume with a shared
// Redis metadata store. That is deliberately the most forgiving environment replication will ever
// see: one inode namespace, one page cache, real POSIX locks. **A failure here is conclusive** and
// applies to every real filesystem and to S3; a pass here is worth nothing on its own, because it
// says nothing about cross-client coherence on a real RWX filesystem.
//
// Why any of this is in doubt: zot's ImageStore serialises repository writes with an in-process
// `*sync.RWMutex` and nothing else — there is no flock anywhere in pkg/storage/imagestore — and
// `GetIndexContent` carries the comment "the caller function MUST lock from outside". A manifest
// push is a read-modify-write of <repo>/index.json, written whole-file to a temp path and renamed.
// Sharding is what made that in-process lock sufficient: exactly one process ever owned a repo.
// Replication removes that invariant.
//
//	cd test/spike && docker compose up -d
//	go test -tags spike ./test/spike/ -v -count=1
package spike

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

var endpoints = []string{
	"http://localhost:5001",
	"http://localhost:5002",
	"http://localhost:5003",
}

func TestMain(m *testing.M) {
	for _, e := range endpoints {
		resp, err := http.Get(e + "/v2/")
		if err != nil || resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "%s is not serving; run `docker compose up -d` in test/spike\n", e)
			os.Exit(1)
		}
		resp.Body.Close()
	}
	os.Exit(m.Run())
}

// --- a minimal OCI push, so the harness controls exactly which instance sees which request ---

const (
	configMediaType   = "application/vnd.oci.image.config.v1+json"
	layerMediaType    = "application/vnd.oci.image.layer.v1.tar"
	manifestMediaType = "application/vnd.oci.image.manifest.v1+json"
)

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// pushBlob uploads one blob monolithically to a specific endpoint.
func pushBlob(endpoint, repo string, body []byte) (string, error) {
	dig := digestOf(body)
	resp, err := http.Post(fmt.Sprintf("%s/v2/%s/blobs/uploads/", endpoint, repo), "", nil)
	if err != nil {
		return "", err
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if loc == "" {
		return "", fmt.Errorf("no upload location (status %d)", resp.StatusCode)
	}
	if !strings.HasPrefix(loc, "http") {
		loc = endpoint + loc
	}
	sep := "?"
	if strings.Contains(loc, "?") {
		sep = "&"
	}
	req, _ := http.NewRequest(http.MethodPut, loc+sep+"digest="+dig, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	put, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer put.Body.Close()
	if put.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(put.Body)
		return "", fmt.Errorf("blob PUT %d: %s", put.StatusCode, msg)
	}
	return dig, nil
}

// pushImage pushes a one-layer image and tags it, entirely through one endpoint.
func pushImage(endpoint, repo, tag string, payload []byte) (string, error) {
	cfg := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	cfgDig, err := pushBlob(endpoint, repo, cfg)
	if err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	layerDig, err := pushBlob(endpoint, repo, payload)
	if err != nil {
		return "", fmt.Errorf("layer: %w", err)
	}

	mf := map[string]any{
		"schemaVersion": 2,
		"mediaType":     manifestMediaType,
		"config":        map[string]any{"mediaType": configMediaType, "digest": cfgDig, "size": len(cfg)},
		"layers": []map[string]any{
			{"mediaType": layerMediaType, "digest": layerDig, "size": len(payload)},
		},
	}
	body, _ := json.Marshal(mf)
	req, _ := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/v2/%s/manifests/%s", endpoint, repo, tag), bytes.NewReader(body))
	req.Header.Set("Content-Type", manifestMediaType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("manifest PUT %d: %s", resp.StatusCode, msg)
	}
	return digestOf(body), nil
}

func listTags(endpoint, repo string) (map[string]bool, error) {
	resp, err := http.Get(fmt.Sprintf("%s/v2/%s/tags/list", endpoint, repo))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tags/list %d", resp.StatusCode)
	}
	var out struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, t := range out.Tags {
		set[t] = true
	}
	return set, nil
}

func headManifest(endpoint, repo, ref string) (int, error) {
	req, _ := http.NewRequest(http.MethodHead, fmt.Sprintf("%s/v2/%s/manifests/%s", endpoint, repo, ref), nil)
	req.Header.Set("Accept", manifestMediaType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// P1 — the lost update, and this test ASSERTS THAT IT HAPPENS.
//
// Two writers push distinct tags into ONE repository through DIFFERENT instances, concurrently.
// Each push is a read-modify-write of the same index.json guarded only by a per-process mutex, so
// tags that returned 201 simply vanish, and every instance then agrees they were never written.
//
// The assertion is inverted on purpose. This measurement is the entire justification for ADR 0041
// — one writer, N read-only replicas — so the thing worth guarding is that the justification still
// holds. If this ever PASSES, one of two things is true: the harness has stopped exercising the
// race and every other result here is void, or zot has learned to coordinate writes across
// processes and the design should be revisited. Both are worth a failing test.
//
// Storage-agnostic: the same race exists on S3, because it is between processes rather than
// between clients of a disk.
func TestTwoMutatorsLoseContent(t *testing.T) {
	// A fresh repository per run. Reusing one would let each run start with the previous run's
	// tags, changing the contention profile and making the failure rate meaningless.
	repo := fmt.Sprintf("spike/lost-update-%d", time.Now().UnixNano())
	const perWorker = 50
	writers := []string{endpoints[0], endpoints[1]}

	var wg sync.WaitGroup
	pushed := make([]map[string]bool, len(writers))
	errs := make([][]error, len(writers))
	for i, ep := range writers {
		pushed[i] = map[string]bool{}
		wg.Add(1)
		go func(i int, ep string) {
			defer wg.Done()
			for n := 0; n < perWorker; n++ {
				tag := fmt.Sprintf("w%d-%03d", i, n)
				if _, err := pushImage(ep, repo, tag, []byte(fmt.Sprintf("payload-%d-%d", i, n))); err != nil {
					errs[i] = append(errs[i], fmt.Errorf("%s: %w", tag, err))
					continue
				}
				pushed[i][tag] = true
			}
		}(i, ep)
	}
	wg.Wait()

	accepted := map[string]bool{}
	rejected := 0
	for i := range writers {
		for _, err := range errs[i] {
			rejected++
			if rejected <= 3 {
				t.Logf("push rejected: %v", err)
			}
		}
		for tag := range pushed[i] {
			accepted[tag] = true
		}
	}
	t.Logf("%d tags returned 201, %d rejected outright", len(accepted), rejected)

	// Give any rename-vs-cache lag a chance; on one volume there should be none.
	time.Sleep(2 * time.Second)

	lost := false
	for _, ep := range endpoints {
		got, err := listTags(ep, repo)
		if err != nil {
			t.Fatalf("%s: %v", ep, err)
		}
		var missing []string
		for tag := range accepted {
			if !got[tag] {
				missing = append(missing, tag)
			}
		}
		if len(missing) > 0 {
			t.Logf("%s lost %d of %d accepted tags (e.g. %v)",
				ep, len(missing), len(accepted), missing[:min(5, len(missing))])
			lost = true
		}
	}

	if !lost && rejected == 0 {
		t.Fatalf("two instances wrote %d tags into one repository concurrently and NOTHING was lost "+
			"or rejected. Either this harness has stopped exercising the race -- in which case every "+
			"other result here is void -- or zot now coordinates writes across processes, and ADR 0041 "+
			"should be revisited.", len(accepted))
	}
	t.Logf("as expected: content was lost or rejected, which is why only one pod may write")
}

// TestSequentialPushesAreNotLost is P1's negative control.
//
// The same tags, the same repository, the same instances — but one at a time. If this fails, the
// harness is broken and the concurrent result above means nothing.
func TestSequentialPushesAreNotLost(t *testing.T) {
	repo := fmt.Sprintf("spike/sequential-control-%d", time.Now().UnixNano())
	accepted := map[string]bool{}
	for n := 0; n < 40; n++ {
		ep := endpoints[n%len(endpoints)]
		tag := fmt.Sprintf("seq-%03d", n)
		if _, err := pushImage(ep, repo, tag, []byte(fmt.Sprintf("seq-%d", n))); err != nil {
			t.Fatalf("%s %s: %v", ep, tag, err)
		}
		accepted[tag] = true
	}
	for _, ep := range endpoints {
		got, err := listTags(ep, repo)
		if err != nil {
			t.Fatalf("%s: %v", ep, err)
		}
		for tag := range accepted {
			if !got[tag] {
				t.Errorf("%s is missing %s from a SEQUENTIAL run; the harness itself is unsound", ep, tag)
			}
		}
	}
}

// TestConcurrentPushesToONEInstance is the control that decides what P1 actually means.
//
// Identical concurrency, identical repository — but both writers hit the SAME instance, so zot's
// in-process *sync.RWMutex is the only thing that has to work. If this passes while the two-instance
// version loses tags, the defect is specifically MULTI-PROCESS and replication is what introduces
// it. If this fails too, zot cannot serialise concurrent pushes into one repository at all, and
// that is a bug that affects this project TODAY at one replica.
func TestConcurrentPushesToOneInstanceAreNotLost(t *testing.T) {
	repo := fmt.Sprintf("spike/one-instance-%d", time.Now().UnixNano())
	const perWorker = 50
	only := endpoints[0]

	var wg sync.WaitGroup
	pushed := make([]map[string]bool, 2)
	var mu sync.Mutex
	for i := 0; i < 2; i++ {
		pushed[i] = map[string]bool{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < perWorker; n++ {
				tag := fmt.Sprintf("s%d-%03d", i, n)
				if _, err := pushImage(only, repo, tag, []byte(fmt.Sprintf("one-%d-%d", i, n))); err != nil {
					t.Errorf("push %s: %v", tag, err)
					continue
				}
				mu.Lock()
				pushed[i][tag] = true
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	accepted := map[string]bool{}
	for i := range pushed {
		for tag := range pushed[i] {
			accepted[tag] = true
		}
	}
	time.Sleep(2 * time.Second)

	got, err := listTags(only, repo)
	if err != nil {
		t.Fatalf("%s: %v", only, err)
	}
	var missing []string
	for tag := range accepted {
		if !got[tag] {
			missing = append(missing, tag)
		}
	}
	if len(missing) > 0 {
		t.Errorf("ONE instance lost %d of %d accepted tags (e.g. %v) -- concurrent pushes are unsafe "+
			"even without replication", len(missing), len(accepted), missing[:min(5, len(missing))])
	}
}

// P3-local — read-after-write across instances.
//
// Push through one instance, immediately HEAD on the others. Two variants, and the split is the
// point: by digest is a new file path, while by tag needs a fresh index.json. On one volume both
// must pass; on a real RWX filesystem the tag variant is the one expected to fail.
//
// It matters because internal/reconciler/published.go HEADs a tag it just pushed and treats 404 as
// authoritative, and internal/controller/attest.go HEADs a manifest immediately after pushing it.
func TestReadAfterWriteAcrossInstances(t *testing.T) {
	repo := fmt.Sprintf("spike/raw-%d", time.Now().UnixNano())
	const iterations = 200

	var byTag, byDigest int
	for n := 0; n < iterations; n++ {
		writer := endpoints[n%len(endpoints)]
		tag := fmt.Sprintf("raw-%04d", n)
		dig, err := pushImage(writer, repo, tag, []byte(fmt.Sprintf("raw-%d", n)))
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		for _, reader := range endpoints {
			if reader == writer {
				continue
			}
			if code, _ := headManifest(reader, repo, tag); code != http.StatusOK {
				byTag++
				if byTag <= 3 {
					t.Errorf("tag %s written on %s reads %d on %s", tag, writer, code, reader)
				}
			}
			if code, _ := headManifest(reader, repo, dig); code != http.StatusOK {
				byDigest++
				if byDigest <= 3 {
					t.Errorf("digest %s written on %s reads %d on %s", dig, writer, code, reader)
				}
			}
		}
	}
	t.Logf("%d iterations: %d stale by tag, %d stale by digest", iterations, byTag, byDigest)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// P2+P6 — concurrent GC over one store, against a refresher keeping content alive.
//
// This is the sharpest probe in the spike, because it tests the retention guarantee itself
// (ADR 0031) under replication. The bargain is: the registry reclaims anything not pulled within
// the window, and the controllers keep live objects alive by re-pulling them. Replication puts
// those two halves in DIFFERENT processes — the refresh lands on whichever instance the load
// balancer chose, while every instance runs its own GC sweep against the same tree.
//
// So it answers two questions at once. Does a pull recorded by one instance protect content from
// another instance's collector (P6, which is what the shared Redis metaDB is for)? And does
// concurrent collection over one store destroy anything (P2)?
//
// Each instance writes its own repository, isolating this from the index.json contention P1 found.
func TestARefreshedImageSurvivesEveryInstancesCollector(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test")
	}
	type ref struct {
		repo, tag, digest string
		payload           []byte
	}
	var accepted []ref
	var mu sync.Mutex
	stamp := time.Now().UnixNano()
	stop := make(chan struct{})

	// The refresher: pulls everything accepted so far, continuously, round-robining instances --
	// exactly what internal/retention/refresher.go does, and deliberately NOT pinned to the
	// instance that wrote the content.
	var refreshes int
	var refresher sync.WaitGroup
	refresher.Add(1)
	go func() {
		defer refresher.Done()
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			mu.Lock()
			snapshot := append([]ref(nil), accepted...)
			mu.Unlock()
			for _, r := range snapshot {
				ep := endpoints[n%len(endpoints)]
				// A GET, not a HEAD: only a pull renews recency.
				if resp, err := http.Get(fmt.Sprintf("%s/v2/%s/manifests/%s", ep, r.repo, r.tag)); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					refreshes++
				}
				n++
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	deadline := time.Now().Add(45 * time.Second)
	var wg sync.WaitGroup
	for i, ep := range endpoints {
		wg.Add(1)
		go func(i int, ep string) {
			defer wg.Done()
			repo := fmt.Sprintf("keep-gc-%d-%d", stamp, i)
			for n := 0; time.Now().Before(deadline); n++ {
				tag := fmt.Sprintf("t%04d", n)
				payload := []byte(fmt.Sprintf("gc-%d-%d-%d", stamp, i, n))
				dig, err := pushImage(ep, repo, tag, payload)
				if err != nil {
					t.Errorf("push %s/%s: %v", repo, tag, err)
					return
				}
				mu.Lock()
				accepted = append(accepted, ref{repo, tag, dig, payload})
				mu.Unlock()
			}
		}(i, ep)
	}
	wg.Wait()

	// Keep refreshing well past the 30s window and several gcInterval sweeps.
	time.Sleep(40 * time.Second)
	close(stop)
	refresher.Wait()
	t.Logf("%d images accepted, %d refresh pulls issued", len(accepted), refreshes)

	var goneByDigest, goneByTag, badLayer int
	for _, r := range accepted {
		if code, _ := headManifest(endpoints[0], r.repo, r.digest); code != http.StatusOK {
			goneByDigest++
			if goneByDigest <= 3 {
				t.Errorf("digest %s (%s:%s) returned %d despite being refreshed", r.digest, r.repo, r.tag, code)
			}
		}
		if code, _ := headManifest(endpoints[1], r.repo, r.tag); code != http.StatusOK {
			goneByTag++
			if goneByTag <= 3 {
				t.Errorf("tag %s:%s returned %d despite being refreshed", r.repo, r.tag, code)
			}
			continue
		}
		resp, err := http.Get(fmt.Sprintf("%s/v2/%s/blobs/%s", endpoints[2], r.repo, digestOf(r.payload)))
		if err != nil {
			t.Fatalf("layer GET: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || digestOf(body) != digestOf(r.payload) {
			badLayer++
			if badLayer <= 3 {
				t.Errorf("layer of %s:%s: status %d, digest match %v",
					r.repo, r.tag, resp.StatusCode, digestOf(body) == digestOf(r.payload))
			}
		}
	}
	t.Logf("after GC: %d gone by digest, %d gone by tag, %d layers missing or corrupt",
		goneByDigest, goneByTag, badLayer)
}

// The writer/reader split: does confining every MUTATION to one instance make the rest safe?
//
// P1 and the refresh probe both trace to the same root cause: <repo>/index.json is a
// read-modify-write guarded only by a per-process mutex, and any instance that pushes OR collects
// is a writer of it. So the hypothesis is that N instances are safe exactly when N-1 of them never
// mutate the store -- `gc: false`, no retention policy, and no pushes routed to them.
//
// Run against the writer/reader compose layout: 5001 pushes and collects, 5002/5003 only serve.
// Reads and refreshes still fan out across all three, which is the point -- pulls are what has to
// survive a node drain.
func TestOnlyOneMutatorMakesTheRestSafe(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test")
	}
	const writer = "http://localhost:5001"
	type ref struct {
		repo, tag, digest string
		payload           []byte
	}
	var accepted []ref
	var mu sync.Mutex
	stamp := time.Now().UnixNano()
	stop := make(chan struct{})

	var refreshes int
	var refresher sync.WaitGroup
	refresher.Add(1)
	go func() {
		defer refresher.Done()
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			mu.Lock()
			snapshot := append([]ref(nil), accepted...)
			mu.Unlock()
			for _, r := range snapshot {
				ep := endpoints[n%len(endpoints)]
				if resp, err := http.Get(fmt.Sprintf("%s/v2/%s/manifests/%s", ep, r.repo, r.tag)); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					refreshes++
				}
				n++
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	// Two concurrent writers, both against the SINGLE mutating instance, into one repository --
	// the exact shape that lost 4% when the writers were two different instances.
	repo := fmt.Sprintf("keep-split-%d", stamp)
	deadline := time.Now().Add(45 * time.Second)
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for n := 0; time.Now().Before(deadline); n++ {
				tag := fmt.Sprintf("w%d-t%04d", w, n)
				payload := []byte(fmt.Sprintf("split-%d-%d-%d", stamp, w, n))
				dig, err := pushImage(writer, repo, tag, payload)
				if err != nil {
					t.Errorf("push %s: %v", tag, err)
					return
				}
				mu.Lock()
				accepted = append(accepted, ref{repo, tag, dig, payload})
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	time.Sleep(40 * time.Second)
	close(stop)
	refresher.Wait()
	t.Logf("%d images accepted, %d refresh pulls issued", len(accepted), refreshes)

	var goneByDigest, goneByTag, badLayer int
	for _, r := range accepted {
		if code, _ := headManifest(endpoints[0], r.repo, r.digest); code != http.StatusOK {
			goneByDigest++
			if goneByDigest <= 3 {
				t.Errorf("digest %s (%s:%s) returned %d despite being refreshed", r.digest, r.repo, r.tag, code)
			}
		}
		// Read from a READER, which is the path a workload pull takes.
		if code, _ := headManifest(endpoints[2], r.repo, r.tag); code != http.StatusOK {
			goneByTag++
			if goneByTag <= 3 {
				t.Errorf("tag %s:%s returned %d from a read replica", r.repo, r.tag, code)
			}
			continue
		}
		resp, err := http.Get(fmt.Sprintf("%s/v2/%s/blobs/%s", endpoints[1], r.repo, digestOf(r.payload)))
		if err != nil {
			t.Fatalf("layer GET: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || digestOf(body) != digestOf(r.payload) {
			badLayer++
			if badLayer <= 3 {
				t.Errorf("layer of %s:%s from a read replica: status %d, digest match %v",
					r.repo, r.tag, resp.StatusCode, digestOf(body) == digestOf(r.payload))
			}
		}
	}
	t.Logf("with a single mutator: %d gone by digest, %d gone by tag, %d layers bad",
		goneByDigest, goneByTag, badLayer)
}

// stopInstance stops one container and returns a function that starts it again.
func stopInstance(t *testing.T, service string) func() {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("docker", append([]string{"compose", "-f", "compose.yaml"}, args...)...)
		cmd.Dir = "."
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("docker compose %v: %v\n%s", args, err, out)
		}
	}
	run("stop", service)
	return func() {
		run("start", service)
		// Wait for it to serve again. Without this the next test pushes into an instance that is
		// still starting and fails for a reason that has nothing to do with what it is testing.
		waitServing(t, serviceEndpoint[service])
	}
}

// serviceEndpoint maps a compose service to the address it publishes.
var serviceEndpoint = map[string]string{
	"zot-0": "http://localhost:5001",
	"zot-1": "http://localhost:5002",
	"zot-2": "http://localhost:5003",
}

func waitServing(t *testing.T, endpoint string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(endpoint + "/v2/"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s did not come back within 30s", endpoint)
}

// pullAll fetches every image through the given endpoints, round-robin, and classifies what came
// back. A 404 means content is MISSING, which is the failure this design exists to prevent; a
// connection error means that instance is gone, which is expected and survivable.
func pullAll(endpoints []string, refs []struct{ repo, tag string }) (ok, missing, unreachable int) {
	client := &http.Client{Timeout: 3 * time.Second}
	for i, r := range refs {
		ep := endpoints[i%len(endpoints)]
		resp, err := client.Get(fmt.Sprintf("%s/v2/%s/manifests/%s", ep, r.repo, r.tag))
		if err != nil {
			unreachable++
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			ok++
		case http.StatusNotFound:
			missing++
		default:
			unreachable++
		}
	}
	return
}

// TestPullsSurviveAnInstanceGoingAway is the whole point of the feature.
//
// The failure that started this work: nodes were cordoned, the registry was rescheduled alongside
// the pods that pull from it, and those pods sat in ErrImagePull. So the claim to test is that any
// SURVIVING instance serves EVERY image.
//
// Two cases, and the second is the one that matters. Losing a read replica is the easy case. Losing
// the WRITER must still leave every image pullable, because the writer is the single point the
// design deliberately keeps — if pulls died with it, replication would have bought nothing.
//
// A 404 is the failure. A connection error against the instance that was deliberately stopped is
// not: that is what a Service removing an endpoint looks like from the outside, and any client
// retries it.
func TestPullsSurviveAnInstanceGoingAway(t *testing.T) {
	const writer = "http://localhost:5001"
	repo := fmt.Sprintf("keep-drain-%d", time.Now().UnixNano())

	var refs []struct{ repo, tag string }
	for n := 0; n < 40; n++ {
		tag := fmt.Sprintf("d%03d", n)
		if _, err := pushImage(writer, repo, tag, []byte(fmt.Sprintf("drain-%s-%d", repo, n))); err != nil {
			t.Fatalf("seeding %s: %v", tag, err)
		}
		refs = append(refs, struct{ repo, tag string }{repo, tag})
	}

	if ok, missing, unreachable := pullAll(endpoints, refs); missing > 0 || ok != len(refs) {
		t.Fatalf("baseline is not clean: %d ok, %d missing, %d unreachable", ok, missing, unreachable)
	}

	t.Run("a read replica goes away", func(t *testing.T) {
		restore := stopInstance(t, "zot-2")
		defer restore()

		survivors := endpoints[:2]
		ok, missing, unreachable := pullAll(survivors, refs)
		t.Logf("survivors served %d, missing %d, unreachable %d", ok, missing, unreachable)
		if missing > 0 {
			t.Errorf("%d images 404ed while a read replica was down; a surviving instance must serve every image", missing)
		}
		if ok != len(refs) {
			t.Errorf("only %d of %d pulls succeeded against the survivors", ok, len(refs))
		}
	})

	// Let the replica rejoin before the next case.
	time.Sleep(3 * time.Second)

	t.Run("the writer goes away", func(t *testing.T) {
		restore := stopInstance(t, "zot-0")
		defer restore()

		readers := endpoints[1:]
		ok, missing, unreachable := pullAll(readers, refs)
		t.Logf("read replicas served %d, missing %d, unreachable %d", ok, missing, unreachable)
		if missing > 0 {
			t.Errorf("%d images 404ed while the WRITER was down — replication bought nothing if pulls "+
				"die with the single writer", missing)
		}
		if ok != len(refs) {
			t.Errorf("only %d of %d pulls succeeded against the read replicas", ok, len(refs))
		}
	})
}

// TestASingleInstanceDoesNotSurviveGoingAway is the negative control, and it is what makes the test
// above mean anything. A drain test that passes on a single instance is measuring nothing.
func TestASingleInstanceDoesNotSurviveGoingAway(t *testing.T) {
	const only = "http://localhost:5001"
	repo := fmt.Sprintf("keep-control-%d", time.Now().UnixNano())

	var refs []struct{ repo, tag string }
	for n := 0; n < 10; n++ {
		tag := fmt.Sprintf("c%03d", n)
		if _, err := pushImage(only, repo, tag, []byte(fmt.Sprintf("ctl-%s-%d", repo, n))); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		refs = append(refs, struct{ repo, tag string }{repo, tag})
	}

	restore := stopInstance(t, "zot-0")
	defer restore()

	ok, missing, unreachable := pullAll([]string{only}, refs)
	t.Logf("single instance down: %d ok, %d missing, %d unreachable", ok, missing, unreachable)
	if ok > 0 {
		t.Fatalf("%d pulls succeeded against an instance that was stopped — the harness is not "+
			"actually taking it down, so the survival test above proves nothing", ok)
	}
}
