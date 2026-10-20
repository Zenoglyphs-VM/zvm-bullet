package fair

import (
	"bytes"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/vrf"
)

func key(t *testing.T) *vrf.PrivateKey {
	k, err := vrf.NewKey(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func bets(xs ...int64) []*big.Int {
	out := make([]*big.Int, len(xs))
	for i, x := range xs {
		out[i] = big.NewInt(x)
	}
	return out
}

func TestAlphaMatchesMainnetShape(t *testing.T) {
	now := time.Unix(1692845778, 757002000)
	got := Alpha(now, bets(10000000000, 10000000000, 10000000000))
	want := "1692845778.757002,[10000000000, 10000000000, 10000000000]"
	if got != want {
		t.Fatalf("alpha %q, want %q", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	k := key(t)
	for size := 2; size <= 12; size++ {
		c, err := New(k, size, bets(5000000000, 7000000000), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if c.Chamber < 0 || c.Chamber >= size {
			t.Fatalf("chamber %d outside a %d-shot revolver", c.Chamber, size)
		}
		got, err := Verify(size, c.Hash, c.Reveal(k.Public()))
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if got.Chamber != c.Chamber || got.Alpha != c.Alpha || !bytes.Equal(got.PublicKey, k.Public()) {
			t.Fatalf("size %d: verify gave %+v", size, got)
		}
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	k := key(t)
	c, err := New(k, 6, bets(100, 200, 300), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reveal := c.Reveal(k.Public())

	// A different commitment: the chamber was not the one committed to.
	other := c.Hash
	other[0] ^= 1
	if _, err := Verify(6, other, reveal); err == nil {
		t.Fatal("verified against another commitment")
	}
	// Another revolver size moves the chamber, so the hash no longer matches
	// (unless beta happens to reduce to the same value, which 5 and 6 cannot
	// both do for every beta; pick a size where it differs).
	for size := 2; size <= 12; size++ {
		if ChamberOf(c.Beta, size) != c.Chamber {
			if _, err := Verify(size, c.Hash, reveal); err == nil {
				t.Fatalf("verified with size %d", size)
			}
			break
		}
	}
	tamper := func(i int, f func(string) string) []string {
		r := append([]string(nil), reveal...)
		r[i] = f(r[i])
		return r
	}
	flip := func(s string) string { // change the last hex digit
		last := s[len(s)-1]
		if last == '0' {
			return s[:len(s)-1] + "1"
		}
		return s[:len(s)-1] + "0"
	}
	cases := map[string][]string{
		"beta":  tamper(0, flip),
		"salt":  tamper(1, flip),
		"key":   tamper(2, flip),
		"proof": tamper(3, flip),
		"alpha": tamper(4, func(s string) string { return strings.Replace(s, "100", "101", 1) }),
	}
	for name, r := range cases {
		if _, err := Verify(6, c.Hash, r); err == nil {
			t.Fatalf("verified with a tampered %s", name)
		}
	}
}

// The chamber must be flat over the revolver: rejection sampling is not
// needed because beta is 512 bits, and the bias of reducing 2^512 values mod
// n is below 2^-500.
func TestChamberIsUniform(t *testing.T) {
	k := key(t)
	const size, n = 6, 6000
	var counts [size]int
	for i := 0; i < n; i++ {
		c, err := New(k, size, bets(int64(i)), time.Unix(int64(i), 0))
		if err != nil {
			t.Fatal(err)
		}
		counts[c.Chamber]++
	}
	for ch, got := range counts {
		// Expected 1000 each; 5 sigma is about 144.
		if got < 850 || got > 1150 {
			t.Fatalf("chamber %d drawn %d times in %d: %v", ch, got, n, counts)
		}
	}
}
