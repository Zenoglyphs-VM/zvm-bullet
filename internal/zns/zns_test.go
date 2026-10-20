package zns

import "testing"

// EIP-137's own vectors, and the addr.reverse node every ENS deployment shares.
func TestNamehash(t *testing.T) {
	for name, want := range map[string]string{
		"":             "0x0000000000000000000000000000000000000000000000000000000000000000",
		"eth":          "0x93cdeb708b7545dc668eb9280176169d1c33cfd8ed6f04690a0bcc88a93fc4ae",
		"foo.eth":      "0xde9b09fd7c5f901e23a3f19fecc54828e9c848539801e86591bd9801b019f84f",
		"addr.reverse": "0x91d1777781884d03a6757a803996e38de2a42967fb37eeaca72729271025a9e2",
		"znn":          "0x6be5efe3a46d2356cb7ec1033d96cb4f426a49b9805d060359fade58800cf633",
	} {
		if got := Namehash(name).Hex(); got != want {
			t.Errorf("namehash(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestNilNames(t *testing.T) {
	var n *Names
	if n.Name([20]byte{1}) != "" {
		t.Fatal("a nil Names must answer no name")
	}
}
