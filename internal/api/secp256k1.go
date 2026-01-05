package api

import (
	"crypto/elliptic"
	"math/big"
	"sync"
)

// secp256k1Curve implements the secp256k1 elliptic curve
type secp256k1Curve struct {
	*elliptic.CurveParams
}

var (
	secp256k1Once  sync.Once
	secp256k1Inst  *secp256k1Curve
)

// secp256k1 returns the secp256k1 curve instance
func secp256k1() elliptic.Curve {
	secp256k1Once.Do(func() {
		secp256k1Inst = &secp256k1Curve{
			CurveParams: &elliptic.CurveParams{
				Name:    "secp256k1",
				BitSize: 256,
			},
		}

		// Prime field
		secp256k1Inst.P, _ = new(big.Int).SetString("fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f", 16)
		// Order of base point
		secp256k1Inst.N, _ = new(big.Int).SetString("fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141", 16)
		// Curve coefficient a = 0
		// Curve coefficient b = 7
		secp256k1Inst.B = big.NewInt(7)
		// Base point (generator)
		secp256k1Inst.Gx, _ = new(big.Int).SetString("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798", 16)
		secp256k1Inst.Gy, _ = new(big.Int).SetString("483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8", 16)
	})
	return secp256k1Inst
}

// Params returns the curve parameters
func (curve *secp256k1Curve) Params() *elliptic.CurveParams {
	return curve.CurveParams
}

// IsOnCurve checks if point (x, y) is on the curve
func (curve *secp256k1Curve) IsOnCurve(x, y *big.Int) bool {
	// y² = x³ + 7 (mod p)
	y2 := new(big.Int).Mul(y, y)
	y2.Mod(y2, curve.P)

	x3 := new(big.Int).Mul(x, x)
	x3.Mul(x3, x)
	x3.Add(x3, curve.B)
	x3.Mod(x3, curve.P)

	return y2.Cmp(x3) == 0
}

// Add returns the sum of (x1, y1) and (x2, y2)
func (curve *secp256k1Curve) Add(x1, y1, x2, y2 *big.Int) (*big.Int, *big.Int) {
	// Handle identity cases
	if x1.Sign() == 0 && y1.Sign() == 0 {
		return x2, y2
	}
	if x2.Sign() == 0 && y2.Sign() == 0 {
		return x1, y1
	}

	// If points are the same, use doubling
	if x1.Cmp(x2) == 0 && y1.Cmp(y2) == 0 {
		return curve.Double(x1, y1)
	}

	// If x1 == x2 but y1 != y2, result is point at infinity
	if x1.Cmp(x2) == 0 {
		return new(big.Int), new(big.Int)
	}

	// λ = (y2 - y1) / (x2 - x1)
	dy := new(big.Int).Sub(y2, y1)
	dx := new(big.Int).Sub(x2, x1)
	dxInv := new(big.Int).ModInverse(dx, curve.P)
	lambda := new(big.Int).Mul(dy, dxInv)
	lambda.Mod(lambda, curve.P)

	// x3 = λ² - x1 - x2
	x3 := new(big.Int).Mul(lambda, lambda)
	x3.Sub(x3, x1)
	x3.Sub(x3, x2)
	x3.Mod(x3, curve.P)

	// y3 = λ(x1 - x3) - y1
	y3 := new(big.Int).Sub(x1, x3)
	y3.Mul(y3, lambda)
	y3.Sub(y3, y1)
	y3.Mod(y3, curve.P)

	return x3, y3
}

// Double returns 2*(x, y)
func (curve *secp256k1Curve) Double(x1, y1 *big.Int) (*big.Int, *big.Int) {
	// If y == 0, result is point at infinity
	if y1.Sign() == 0 {
		return new(big.Int), new(big.Int)
	}

	// λ = (3x₁² + a) / (2y₁), where a = 0 for secp256k1
	x1Sq := new(big.Int).Mul(x1, x1)
	x1Sq.Mul(x1Sq, big.NewInt(3))
	x1Sq.Mod(x1Sq, curve.P)

	y1Double := new(big.Int).Mul(y1, big.NewInt(2))
	y1DoubleInv := new(big.Int).ModInverse(y1Double, curve.P)
	lambda := new(big.Int).Mul(x1Sq, y1DoubleInv)
	lambda.Mod(lambda, curve.P)

	// x3 = λ² - 2x1
	x3 := new(big.Int).Mul(lambda, lambda)
	x3.Sub(x3, new(big.Int).Mul(x1, big.NewInt(2)))
	x3.Mod(x3, curve.P)

	// y3 = λ(x1 - x3) - y1
	y3 := new(big.Int).Sub(x1, x3)
	y3.Mul(y3, lambda)
	y3.Sub(y3, y1)
	y3.Mod(y3, curve.P)

	return x3, y3
}

// ScalarMult returns k*(Bx, By)
func (curve *secp256k1Curve) ScalarMult(Bx, By *big.Int, k []byte) (*big.Int, *big.Int) {
	// Double-and-add algorithm
	Rx, Ry := new(big.Int), new(big.Int)
	Px, Py := Bx, By

	for i := len(k) - 1; i >= 0; i-- {
		for j := 0; j < 8; j++ {
			if (k[i]>>j)&1 == 1 {
				Rx, Ry = curve.Add(Rx, Ry, Px, Py)
			}
			Px, Py = curve.Double(Px, Py)
		}
	}

	return Rx, Ry
}

// ScalarBaseMult returns k*G, where G is the base point
func (curve *secp256k1Curve) ScalarBaseMult(k []byte) (*big.Int, *big.Int) {
	return curve.ScalarMult(curve.Gx, curve.Gy, k)
}
