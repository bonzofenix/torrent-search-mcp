package torrent

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"unicode/utf8"
)

// base62Charset matches pybase62's default charset, so ids produced by the
// Python server decode here and vice versa.
const base62Charset = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Compress zlib-compresses text and encodes it as base62 (Compress62.compress).
func Compress(text string) string {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, 6)
	_, _ = w.Write([]byte(text))
	_ = w.Close()
	return encodeBytes(buf.Bytes())
}

// Decompress reverses Compress (Compress62.decompress).
func Decompress(compressed string) (string, error) {
	raw, err := decodeBytes(compressed)
	if err != nil {
		return "", err
	}
	r, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(out) {
		return "", errors.New("decompressed query is not valid UTF-8")
	}
	return string(out), nil
}

// encodeBytes is pybase62.encodebytes: leading null bytes are written as
// "0" + a count digit, the rest as a big-endian base62 integer.
func encodeBytes(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}
	n, r := zeros/(len(base62Charset)-1), zeros%(len(base62Charset)-1)
	padding := strings.Repeat("0"+base62Charset[len(base62Charset)-1:], n)
	if r > 0 {
		padding += "0" + string(base62Charset[r])
	}
	if zeros == len(b) {
		return padding
	}
	value := new(big.Int).SetBytes(b)
	base := big.NewInt(62)
	mod := new(big.Int)
	var digits []byte
	for value.Sign() > 0 {
		value.DivMod(value, base, mod)
		digits = append(digits, base62Charset[mod.Int64()])
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return padding + string(digits)
}

// decodeBytes is pybase62.decodebytes.
func decodeBytes(encoded string) ([]byte, error) {
	var leading []byte
	for strings.HasPrefix(encoded, "0") && len(encoded) >= 2 {
		count := strings.IndexByte(base62Charset, encoded[1])
		if count < 0 {
			return nil, fmt.Errorf("base62: invalid character (%c)", encoded[1])
		}
		leading = append(leading, make([]byte, count)...)
		encoded = encoded[2:]
	}
	value := new(big.Int)
	base := big.NewInt(62)
	for i := 0; i < len(encoded); i++ {
		digit := strings.IndexByte(base62Charset, encoded[i])
		if digit < 0 {
			return nil, fmt.Errorf("base62: invalid character (%c)", encoded[i])
		}
		value.Mul(value, base)
		value.Add(value, big.NewInt(int64(digit)))
	}
	return append(leading, value.Bytes()...), nil
}
