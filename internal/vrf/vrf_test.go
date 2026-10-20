package vrf

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// RFC 9381 Appendix B.3, Examples 16-18.
var vectors = []struct{ sk, pk, alpha, pi, beta string }{
	{
		"9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60",
		"d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a",
		"",
		"8657106690b5526245a92b003bb079ccd1a92130477671f6fc01ad16f26f723f26f8a57ccaed74ee1b190bed1f479d9727d2d0f9b005a6e456a35d4fb0daab1268a1b0db10836d9826a528ca76567805",
		"90cf1df3b703cce59e2a35b925d411164068269d7b2d29f3301c03dd757876ff66b71dda49d2de59d03450451af026798e8f81cd2e333de5cdf4f3e140fdd8ae",
	},
	{
		"4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb",
		"3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c",
		"72",
		"f3141cd382dc42909d19ec5110469e4feae18300e94f304590abdced48aed5933bf0864a62558b3ed7f2fea45c92a465301b3bbf5e3e54ddf2d935be3b67926da3ef39226bbc355bdc9850112c8f4b02",
		"eb4440665d3891d668e7e0fcaf587f1b4bd7fbfe99d0eb2211ccec90496310eb5e33821bc613efb94db5e5b54c70a848a0bef4553a41befc57663b56373a5031",
	},
	{
		"c5aa8df43f9f837bedb7442f31dcb7b166d38535076f094b85ce3a2e0b4458f7",
		"fc51cd8e6218a1a38da47ed00230f0580816ed13ba3303ac5deb911548908025",
		"af82",
		"9bc0f79119cc5604bf02d23b4caede71393cedfbb191434dd016d30177ccbf8096bb474e53895c362d8628ee9f9ea3c0e52c7a5c691b6c18c9979866568add7a2d41b00b05081ed0f58ee5e31b3a970e",
		"645427e5d00c62a23fb703732fa5d892940935942101e456ecca7bb217c61c452118fec1219202a0edcf038bb6373241578be7217ba85a2687f7a0310b2df19f",
	},
}

func h(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRFC9381Vectors(t *testing.T) {
	for i, v := range vectors {
		key, err := NewKey(h(t, v.sk))
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(key.Public()); got != v.pk {
			t.Fatalf("vector %d: pk %s, want %s", i, got, v.pk)
		}
		proof, beta, err := key.Prove(h(t, v.alpha))
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(proof); got != v.pi {
			t.Fatalf("vector %d: pi %s, want %s", i, got, v.pi)
		}
		if got := hex.EncodeToString(beta); got != v.beta {
			t.Fatalf("vector %d: beta %s, want %s", i, got, v.beta)
		}
		out, err := Verify(h(t, v.pk), h(t, v.alpha), h(t, v.pi))
		if err != nil {
			t.Fatalf("vector %d: verify: %v", i, err)
		}
		if !bytes.Equal(out, beta) {
			t.Fatalf("vector %d: verify returned a different beta", i)
		}
	}
}

func TestVerifyRejects(t *testing.T) {
	v := vectors[1]
	pk, alpha, pi := h(t, v.pk), h(t, v.alpha), h(t, v.pi)

	if _, err := Verify(pk, []byte{0x73}, pi); err == nil {
		t.Fatal("verified against another alpha")
	}
	if _, err := Verify(h(t, vectors[0].pk), alpha, pi); err == nil {
		t.Fatal("verified against another key")
	}
	for _, at := range []int{0, 31, 32, 47, 48, 79} {
		bad := append([]byte(nil), pi...)
		bad[at] ^= 0x01
		if _, err := Verify(pk, alpha, bad); err == nil {
			t.Fatalf("verified with byte %d flipped", at)
		}
	}
	// The identity point, and a point of order 8, as public keys.
	identity := make([]byte, 32)
	identity[0] = 1
	if _, err := Verify(identity, alpha, pi); err == nil {
		t.Fatal("verified against the identity as a key")
	}
	order8 := h(t, "26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05")
	if _, err := Verify(order8, alpha, pi); err == nil {
		t.Fatal("verified against a small-order key")
	}
	// s at or above the group order must be refused rather than reduced.
	bad := append([]byte(nil), pi...)
	copy(bad[48:], h(t, "edd3f55c1a631258d69cf7a2def9de1400000000000000000000000000000010"))
	if _, err := Verify(pk, alpha, bad); err == nil {
		t.Fatal("verified with s = q")
	}
}
