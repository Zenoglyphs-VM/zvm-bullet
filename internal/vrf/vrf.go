// Package vrf is ECVRF-EDWARDS25519-SHA512-TAI from RFC 9381: the verifiable
// random function behind the bullet's chamber.
//
// The original Bullet Game names Algorand's library as its reference prover,
// which implements ECVRF-EDWARDS25519-SHA512-ELL2 from draft 03. This is the
// RFC's other edwards25519 suite. Proofs and outputs have the same shapes
// (an 80-byte proof, a 64-byte beta, a 32-byte public key), and it hashes to
// the curve by try-and-increment instead of Elligator2, which needs no
// hash-to-curve machinery and is checked here against the RFC's own test
// vectors (Appendix B.3). Try-and-increment's running time depends on alpha,
// which RFC 9381 section 7.7 says matters only when alpha is secret; here alpha
// is published in the reveal.
package vrf

import (
	"crypto/sha512"
	"crypto/subtle"
	"errors"

	"filippo.io/edwards25519"
)

const (
	suite = 0x03

	// Sizes, in bytes.
	PublicKeySize = 32
	SeedSize      = 32
	ProofSize     = 80 // Gamma (32) || c (16) || s (32)
	OutputSize    = 64
	cLen          = 16
)

// PrivateKey is an RFC 8032 secret key: a 32-byte seed, from which the secret
// scalar and the public key are derived.
type PrivateKey struct {
	seed []byte
	x    *edwards25519.Scalar
	pk   []byte
}

// NewKey derives the key pair from a 32-byte seed.
func NewKey(seed []byte) (*PrivateKey, error) {
	if len(seed) != SeedSize {
		return nil, errors.New("vrf: seed must be 32 bytes")
	}
	h := sha512.Sum512(seed)
	x, err := new(edwards25519.Scalar).SetBytesWithClamping(h[:32])
	if err != nil {
		return nil, err
	}
	y := new(edwards25519.Point).ScalarBaseMult(x)
	return &PrivateKey{seed: append([]byte(nil), seed...), x: x, pk: y.Bytes()}, nil
}

// Public returns the 32-byte public key.
func (k *PrivateKey) Public() []byte { return append([]byte(nil), k.pk...) }

// Prove returns the proof for alpha and the output it commits to.
func (k *PrivateKey) Prove(alpha []byte) (proof, beta []byte, err error) {
	h, err := encodeToCurve(k.pk, alpha)
	if err != nil {
		return nil, nil, err
	}
	hString := h.Bytes()
	gamma := new(edwards25519.Point).ScalarMult(k.x, h)

	// Nonce, RFC 9381 5.4.2.2: from the second half of the hashed secret key.
	hsk := sha512.Sum512(k.seed)
	kh := sha512.New()
	kh.Write(hsk[32:])
	kh.Write(hString)
	nonce, err := new(edwards25519.Scalar).SetUniformBytes(kh.Sum(nil))
	if err != nil {
		return nil, nil, err
	}

	kB := new(edwards25519.Point).ScalarBaseMult(nonce)
	kH := new(edwards25519.Point).ScalarMult(nonce, h)
	cBytes := challenge(k.pk, hString, gamma.Bytes(), kB.Bytes(), kH.Bytes())
	c, err := scalarFromShort(cBytes)
	if err != nil {
		return nil, nil, err
	}
	s := new(edwards25519.Scalar).MultiplyAdd(c, k.x, nonce) // k + c*x mod q

	proof = make([]byte, 0, ProofSize)
	proof = append(proof, gamma.Bytes()...)
	proof = append(proof, cBytes...)
	proof = append(proof, s.Bytes()...)
	return proof, proofToHash(gamma), nil
}

// Verify checks proof against the public key and alpha, and returns beta if it
// is valid. It validates the key (RFC 9381 5.4.5): a key of small order would
// let one proof verify for many outputs.
func Verify(publicKey, alpha, proof []byte) ([]byte, error) {
	if len(publicKey) != PublicKeySize {
		return nil, errors.New("vrf: public key must be 32 bytes")
	}
	if len(proof) != ProofSize {
		return nil, errors.New("vrf: proof must be 80 bytes")
	}
	y, err := decodePoint(publicKey)
	if err != nil {
		return nil, errors.New("vrf: public key is not a point")
	}
	if new(edwards25519.Point).MultByCofactor(y).Equal(edwards25519.NewIdentityPoint()) == 1 {
		return nil, errors.New("vrf: public key has small order")
	}
	gamma, err := decodePoint(proof[:32])
	if err != nil {
		return nil, errors.New("vrf: Gamma is not a point")
	}
	c, err := scalarFromShort(proof[32 : 32+cLen])
	if err != nil {
		return nil, err
	}
	s, err := new(edwards25519.Scalar).SetCanonicalBytes(proof[32+cLen:])
	if err != nil {
		return nil, errors.New("vrf: s is not below the group order")
	}
	h, err := encodeToCurve(publicKey, alpha)
	if err != nil {
		return nil, err
	}
	negC := new(edwards25519.Scalar).Negate(c)
	// U = s*B - c*Y, V = s*H - c*Gamma
	u := new(edwards25519.Point).VarTimeDoubleScalarBaseMult(negC, y, s)
	v := new(edwards25519.Point).Add(
		new(edwards25519.Point).ScalarMult(s, h),
		new(edwards25519.Point).ScalarMult(negC, gamma))
	c2 := challenge(publicKey, h.Bytes(), proof[:32], u.Bytes(), v.Bytes())
	if subtle.ConstantTimeCompare(c2, proof[32:32+cLen]) != 1 {
		return nil, errors.New("vrf: proof does not verify")
	}
	return proofToHash(gamma), nil
}

// encodeToCurve is RFC 9381 5.4.1.1, try-and-increment, with the public key as
// the salt.
func encodeToCurve(salt, alpha []byte) (*edwards25519.Point, error) {
	for ctr := 0; ctr < 256; ctr++ {
		h := sha512.New()
		h.Write([]byte{suite, 0x01})
		h.Write(salt)
		h.Write(alpha)
		h.Write([]byte{byte(ctr), 0x00})
		sum := h.Sum(nil)
		p, err := decodePoint(sum[:32])
		if err != nil {
			continue
		}
		p.MultByCofactor(p)
		if p.Equal(edwards25519.NewIdentityPoint()) == 1 {
			continue
		}
		return p, nil
	}
	return nil, errors.New("vrf: no point found in 256 tries")
}

// decodePoint is RFC 8032 5.1.3 decoding, which refuses non-canonical
// encodings. filippo.io/edwards25519 accepts some of those (a y that is not
// reduced, or x = 0 with the sign bit set), so an encoding is taken only if it
// is exactly what the decoded point encodes to.
func decodePoint(b []byte) (*edwards25519.Point, error) {
	p, err := new(edwards25519.Point).SetBytes(b)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(p.Bytes(), b) != 1 {
		return nil, errors.New("non-canonical point encoding")
	}
	return p, nil
}

// challenge is RFC 9381 5.4.3; it returns the truncated 16-byte string.
func challenge(points ...[]byte) []byte {
	h := sha512.New()
	h.Write([]byte{suite, 0x02})
	for _, p := range points {
		h.Write(p)
	}
	h.Write([]byte{0x00})
	return h.Sum(nil)[:cLen]
}

// proofToHash is RFC 9381 5.2 steps 4-6.
func proofToHash(gamma *edwards25519.Point) []byte {
	h := sha512.New()
	h.Write([]byte{suite, 0x03})
	h.Write(new(edwards25519.Point).MultByCofactor(gamma).Bytes())
	h.Write([]byte{0x00})
	return h.Sum(nil)
}

// scalarFromShort reads a little-endian integer shorter than 32 bytes as a
// scalar. At 16 bytes it is always below the group order, so it is canonical.
func scalarFromShort(b []byte) (*edwards25519.Scalar, error) {
	var buf [32]byte
	copy(buf[:], b)
	return new(edwards25519.Scalar).SetCanonicalBytes(buf[:])
}
