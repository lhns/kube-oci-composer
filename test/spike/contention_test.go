//go:build spike

package spike

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// Do concurrent uploads to one registry serialise, and how much of it is dedupe?
//
// The evidence behind ADR 0047, which has the reasoning. Measured here against zot v2.1.20 on FAST
// local storage with 1MB blobs -- the least favourable conditions for showing it:
//
//	                single upload     twenty concurrent (min-max)
//	dedupe on       0.366s            3.94s - 6.06s
//	dedupe off      0.341s            1.01s - 3.93s
//
// Twenty SMALL pushes already burn a third of a 60s budget, so a large layer on slower storage
// passes it having transferred nothing -- the reported failure exactly. And dedupe only cuts the
// worst case by about a third, because InitRepo takes the same lock: readTimeout is the fix, dedupe
// is a knob.
//
// Run it the way the replication spike is run:
//
//	cd test/spike && docker compose up -d
//	go test -tags spike -run Contention -v ./test/spike/
const (
	// smallBlob keeps the body transfer negligible, so what is measured is the lock rather than
	// the network. A large blob hides the effect: the transfer happens BEFORE the lock is taken,
	// so two big concurrent uploads overlap happily and look fine.
	smallBlob = 1 << 20
	// concurrency is where the effect becomes unmistakable. At two it is invisible, which is how
	// the first attempt at this measurement concluded, wrongly, that it did not reproduce.
	concurrency = 20
)

// TestConcurrentUploadsSerialise is the claim ADR 0047 rests on.
func TestConcurrentUploadsSerialise(t *testing.T) {
	endpoint := endpoints[0]
	body := make([]byte, smallBlob)
	for i := range body {
		body[i] = byte(i)
	}

	// One alone, as the baseline the rest is measured against.
	repo := fmt.Sprintf("contention-solo-%d", time.Now().UnixNano())
	start := time.Now()
	if _, err := pushBlob(endpoint, repo, body); err != nil {
		t.Fatalf("baseline push: %v", err)
	}
	solo := time.Since(start)

	// The same blob into a fresh repository each time, so every push pays InitRepo AND, once the
	// blob exists, DedupeBlob -- which is what a fleet of builds sharing a base layer does.
	took := make([]time.Duration, concurrency)
	var wg sync.WaitGroup
	for i := range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := fmt.Sprintf("contention-%d-%d", time.Now().UnixNano(), i)
			s := time.Now()
			if _, err := pushBlob(endpoint, r, body); err != nil {
				t.Errorf("concurrent push %d: %v", i, err)
			}
			took[i] = time.Since(s)
		}()
	}
	wg.Wait()

	sort.Slice(took, func(a, b int) bool { return took[a] < took[b] })
	slowest := took[len(took)-1]
	t.Logf("solo %v; %d concurrent: fastest %v, slowest %v", solo, concurrency, took[0], slowest)

	// Deliberately a loose bound. The point is not a threshold, it is that concurrency costs
	// something enormous rather than something marginal -- and a bound tight enough to be precise
	// would be a flake on a busy machine.
	if slowest < 5*solo {
		t.Errorf("expected concurrent uploads to serialise; slowest %v is under 5x solo %v.\n"+
			"If zot has started overlapping uploads, ADR 0047's reasoning needs revisiting.",
			slowest, solo)
	}
}
