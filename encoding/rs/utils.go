package rs

import (
	"errors"
	"fmt"
	"math"

	"github.com/Layr-Labs/eigenda/encoding"
	rb "github.com/Layr-Labs/eigenda/encoding/utils/reverseBits"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
)

// ToFrArray accept a byte array as an input, and converts it to an array of field elements
//
// TODO (litt3): it would be nice to rename this to "DeserializeFieldElements", as the counterpart to "SerializeFieldElements",
//  but doing so would be a very large diff. I'm leaving this comment as a potential future cleanup.
func ToFrArray(inputData []byte) ([]fr.Element, error) {
	bytes := padToBytesPerSymbol(inputData)

	elementCount := len(bytes) / encoding.BYTES_PER_SYMBOL
	outputElements := make([]fr.Element, elementCount)
	for i := 0; i < elementCount; i++ {
		destinationStartIndex := i * encoding.BYTES_PER_SYMBOL
		destinationEndIndex := destinationStartIndex + encoding.BYTES_PER_SYMBOL

		err := outputElements[i].SetBytesCanonical(bytes[destinationStartIndex:destinationEndIndex])
		if err != nil {
			return nil, fmt.Errorf("fr set bytes canonical: %w", err)
		}
	}

	return outputElements, nil
}

// SerializeFieldElements accepts an array of field elements, and serializes it to an array of bytes
func SerializeFieldElements(fieldElements []fr.Element) []byte {
	outputBytes := make([]byte, len(fieldElements)*encoding.BYTES_PER_SYMBOL)

	for i := 0; i < len(fieldElements); i++ {
		destinationStartIndex := i * encoding.BYTES_PER_SYMBOL
		destinationEndIndex := destinationStartIndex + encoding.BYTES_PER_SYMBOL

		fieldElementBytes := fieldElements[i].Bytes()

		copy(outputBytes[destinationStartIndex:destinationEndIndex], fieldElementBytes[:])
	}

	return outputBytes
}

// padToBytesPerSymbol accepts input bytes, and returns the bytes padded to a multiple of encoding.BYTES_PER_SYMBOL
func padToBytesPerSymbol(inputBytes []byte) []byte {
	remainder := len(inputBytes) % encoding.BYTES_PER_SYMBOL

	if remainder == 0 {
		// no padding necessary, since bytes are already a multiple of BYTES_PER_SYMBOL
		return inputBytes
	} else {
		necessaryPadding := encoding.BYTES_PER_SYMBOL - remainder
		return append(inputBytes, make([]byte, necessaryPadding)...)
	}
}

// ToByteArray converts a list of Fr to a byte array
func ToByteArray(dataFr []fr.Element, maxDataSize uint64) []byte {
	n := len(dataFr)
	dataSize := int(math.Min(
		float64(n*encoding.BYTES_PER_SYMBOL),
		float64(maxDataSize),
	))
	data := make([]byte, dataSize)
	for i := 0; i < n; i++ {
		v := dataFr[i].Bytes()

		start := i * encoding.BYTES_PER_SYMBOL
		end := (i + 1) * encoding.BYTES_PER_SYMBOL

		if uint64(end) > maxDataSize {
			copy(data[start:maxDataSize], v[:])
			break
		} else {
			copy(data[start:end], v[:])
		}
	}

	return data
}

func GetNumElement(dataLen uint64, CS int) uint64 {
	numEle := int(math.Ceil(float64(dataLen) / float64(CS)))
	return uint64(numEle)
}

// helper function
func RoundUpDivision(a, b uint64) uint64 {
	return uint64(math.Ceil(float64(a) / float64(b)))
}

func NextPowerOf2(d uint64) uint64 {
	nextPower := math.Ceil(math.Log2(float64(d)))
	return uint64(math.Pow(2.0, nextPower))
}

// This function is used by user to get the leading coset for a frame, where i is frame index
func GetLeadingCosetIndex(i uint64, numChunks uint64) (uint32, error) {

	if i < numChunks {
		j := rb.ReverseBitsLimited(uint32(numChunks), uint32(i))
		return j, nil
	} else {
		return 0, errors.New("cannot create number of frame higher than possible")
	}
}
