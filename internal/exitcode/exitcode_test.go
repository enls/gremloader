package exitcode

import "testing"

func TestWorst(t *testing.T) {
	for _, c := range []struct {
		in   []int
		want int
	}{
		{nil, OK},
		{[]int{OK, OK}, OK},
		{[]int{OK, UpdatesAvailable}, UpdatesAvailable},
		{[]int{UpdatesAvailable, Failed}, Failed},
		{[]int{Failed, Offline}, Offline},
		{[]int{Offline, VerifyFailed, Failed}, VerifyFailed},
		{[]int{VerifyFailed, NeedsRepair, Offline}, NeedsRepair},
		{[]int{NeedsRepair, UpdatesAvailable}, NeedsRepair},
	} {
		if got := Worst(c.in...); got != c.want {
			t.Errorf("Worst(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
