package qr

// rs.go is the Reed-Solomon error-correction half of the encoder, over
// GF(256) with the QR primitive polynomial x^8 + x^4 + x^3 + x^2 + 1 (0x11D).
//
// It is separated from qr.go because it is the part with no QR in it: given a
// data block and a parity length it produces the parity bytes, and it is
// checkable on its own against the ISO worked example.

// expTable and logTable are the GF(256) antilog/log tables, built once at
// package init. They are the whole reason multiplication is a table lookup
// rather than a bit-by-bit carry-less multiply.
var (
	expTable [512]byte
	logTable [256]byte
)

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		expTable[i] = byte(x)
		logTable[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	// The upper half repeats the cycle so a product of two logs (max 254+254)
	// can be looked up without a modulo.
	for i := 255; i < 512; i++ {
		expTable[i] = expTable[i-255]
	}
}

// gfMul multiplies two GF(256) elements. Zero is absorbing — and it has to be
// special-cased, because log(0) is undefined and the table stores 0 for it,
// which would otherwise silently read as log(1).
func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return expTable[int(logTable[a])+int(logTable[b])]
}

// generatorPoly returns the degree-`degree` RS generator polynomial
// (x - a^0)(x - a^1)...(x - a^(degree-1)), coefficients high-order first.
func generatorPoly(degree int) []byte {
	poly := []byte{1}
	for i := 0; i < degree; i++ {
		next := make([]byte, len(poly)+1)
		for j, c := range poly {
			next[j] ^= c
			next[j+1] ^= gfMul(c, expTable[i])
		}
		poly = next
	}
	return poly
}

// reedSolomon returns the `ecLen` parity bytes for one data block: the
// remainder of data * x^ecLen divided by the generator polynomial.
func reedSolomon(data []byte, ecLen int) []byte {
	gen := generatorPoly(ecLen)
	remainder := make([]byte, ecLen)
	for _, d := range data {
		factor := d ^ remainder[0]
		copy(remainder, remainder[1:])
		remainder[ecLen-1] = 0
		for i := 0; i < ecLen; i++ {
			remainder[i] ^= gfMul(gen[i+1], factor)
		}
	}
	return remainder
}
