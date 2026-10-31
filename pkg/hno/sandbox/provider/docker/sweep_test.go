package docker

import (
	"fmt"
	"os"
	"testing"
)

// TestOwnerAliveRejectsWhatItCannotVerify pins the classification that "not in my
// process's map" cannot establish. Another process's stopped container is that process's
// working data -- between create and start, or idle on disk -- and a concurrent gate run
// measured exactly such a container being pruned away.
//
// Liveness is only provable for a label naming this host and a pid that exists. Anything
// else must come back false, which leaves the container classified by its running state
// rather than by a guess.
func TestOwnerAliveRejectsWhatItCannotVerify(t *testing.T) {
	here := hostOfOwnerID()

	if !ownerAlive(fmt.Sprintf("%s/%d/nonce", here, os.Getpid())) {
		t.Fatal("this process's own owner id must count as alive")
	}
	if ownerAlive(fmt.Sprintf("%s/999999999/nonce", here)) {
		t.Fatal("a dead pid must not count as alive")
	}
	// A pid that belongs to a live process but not to a sandbox run: still liveness
	// evidence, which is why the nonce is carried for recycled-pid protection rather
	// than being checked against a table this process cannot see.
	if ownerAlive("some-other-host/4242/nonce") {
		t.Fatal("a label from another host proves nothing about liveness here")
	}
	for _, bad := range []string{"", "garbage", here + "/nopid/nonce", here + "/0/nonce", here + "/-1/nonce"} {
		if ownerAlive(bad) {
			t.Fatalf("owner label %q must not be treated as alive", bad)
		}
	}
}

func hostOfOwnerID() string {
	host, _, _ := splitOwner(ownerID)
	return host
}
